package storage

import (
	"bytes"
	"errors"
	"fmt"
	"log"

	"github.com/coreos/etcd/Godeps/_workspace/src/github.com/google/btree"
)

var (
	ErrReversionNotFound = errors.New("stroage: reversion not found")
)

// keyIndex stores the reversion of an key in the backend.
// Each keyIndex has at least one key generation.
// Each generation might have several key versions.
// Tombstone on a key appends an tombstone version at the end
// of the current generation and creates a new empty generation.
// Each version of a key has an index pointing to the backend.
//
// For example: put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key "foo"
// generate a keyIndex:
// key:     "foo"
// rev: 5
// generations:
//    {empty}
//    {4.0, 5.0(t)}
//    {1.0, 2.0, 3.0(t)}
//
// Compact a keyIndex removes the versions with smaller or equal to
// rev except the largest one. If the generations becomes empty
// during compaction, it will be removed. if all the generations get
// removed, the keyIndex Should be removed.

// For example:
// compact(2) on the previous example
// generations:
//    {empty}
//    {4.0, 5.0(t)}
//    {2.0, 3.0(t)}
//
// compact(4)
// generations:
//    {empty}
//    {4.0, 5.0(t)}
//
// compact(5):
// generations:
//    {empty} -> key SHOULD be removed.
//
// compact(6):
// generations:
//    {empty} -> key SHOULD be removed.
type keyIndex struct {
	key         []byte
	rev         int64
	generations []generation
}

// put puts a reversion to the keyIndex.
func (ki *keyIndex) put(rev int64, subrev int64) {
	if rev < ki.rev {
		log.Panicf("store.keyindex: put with unexpected smaller reversion [%d / %d]", rev, ki.rev)
	}
	if len(ki.generations) == 0 {
		ki.generations = append(ki.generations, generation{})
	}
	g := &ki.generations[len(ki.generations)-1]
	g.revs = append(g.revs, reversion{rev, subrev})
	g.ver++
	ki.rev = rev
}

// tombstone puts a reversion, pointing to a tombstone, to the keyIndex.
// It also creates a new empty generation in the keyIndex.
func (ki *keyIndex) tombstone(rev int64, subrev int64) {
	if ki.isEmpty() {
		log.Panicf("store.keyindex: unexpected tombstone on empty keyIndex %s", string(ki.key))
	}
	ki.put(rev, subrev)
	ki.generations = append(ki.generations, generation{})
}

// get gets the reversion of the key that satisfies the given atRev.
// Rev must be higher than or equal to the given atRev.
func (ki *keyIndex) get(atRev int64) (rev reversion, err error) {
	if ki.isEmpty() {
		log.Panicf("store.keyindex: unexpected get on empty keyIndex %s", string(ki.key))
	}
	g := ki.findGeneration(atRev)
	if g.isEmpty() {
		return reversion{}, ErrReversionNotFound
	}

	f := func(rev reversion) bool {
		if rev.main <= atRev {
			return false
		}
		return true
	}

	n := g.walk(f)
	if n != -1 {
		return g.revs[n], nil
	}

	return reversion{}, ErrReversionNotFound
}

// compact compacts a keyIndex by removing the versions with smaller or equal
// reversion than the given atRev except the largest one (If the largest one is
// a tombstone, it will not be kept).
// If a generation becomes empty during compaction, it will be removed.
func (ki *keyIndex) compact(atRev int64, available map[reversion]struct{}) {
	if ki.isEmpty() {
		log.Panic("store.keyindex: unexpected compact on empty keyIndex %s", string(ki.key))
	}

	// walk until reaching the first reversion that has an reversion smaller or equal to
	// the atReversion.
	// add it to the available map
	f := func(rev reversion) bool {
		if rev.main <= atRev {
			available[rev] = struct{}{}
			return false
		}
		return true
	}

	g := ki.findGeneration(atRev)
	if g == nil {
		return
	}

	i := 0
	for i <= len(ki.generations)-1 {
		wg := &ki.generations[i]
		if wg == g {
			break
		}
		i++
	}

	if !g.isEmpty() {
		n := g.walk(f)
		// remove the previous contents.
		if n != -1 {
			g.revs = g.revs[n:]
		}
		// remove any tombstone
		if len(g.revs) == 1 && i != len(ki.generations)-1 {
			delete(available, g.revs[0])
			i++
		}
	}
	// remove the previous generations.
	ki.generations = ki.generations[i:]
	return
}

func (ki *keyIndex) isEmpty() bool {
	return len(ki.generations) == 1 && ki.generations[0].isEmpty()
}

// findGeneartion finds out the generation of the keyIndex that the
// given index belongs to.
func (ki *keyIndex) findGeneration(rev int64) *generation {
	cg := len(ki.generations) - 1

	for cg >= 0 {
		if len(ki.generations[cg].revs) == 0 {
			cg--
			continue
		}
		g := ki.generations[cg]
		if g.revs[0].main <= rev {
			return &ki.generations[cg]
		}
		cg--
	}
	return nil
}

func (a *keyIndex) Less(b btree.Item) bool {
	return bytes.Compare(a.key, b.(*keyIndex).key) == -1
}

func (a *keyIndex) equal(b *keyIndex) bool {
	if !bytes.Equal(a.key, b.key) {
		return false
	}
	if a.rev != b.rev {
		return false
	}
	if len(a.generations) != len(b.generations) {
		return false
	}
	for i := range a.generations {
		ag, bg := a.generations[i], b.generations[i]
		if !ag.equal(bg) {
			return false
		}
	}
	return true
}

func (ki *keyIndex) String() string {
	var s string
	for _, g := range ki.generations {
		s += g.String()
	}
	return s
}

type generation struct {
	ver  int64
	revs []reversion
}

func (g *generation) isEmpty() bool { return g == nil || len(g.revs) == 0 }

// walk walks through the reversions in the generation in ascending order.
// It passes the revision to the given function.
// walk returns until: 1. it finishs walking all pairs 2. the function returns false.
// walk returns the position at where it stopped. If it stopped after
// finishing walking, -1 will be returned.
func (g *generation) walk(f func(rev reversion) bool) int {
	l := len(g.revs)
	for i := range g.revs {
		ok := f(g.revs[l-i-1])
		if !ok {
			return l - i - 1
		}
	}
	return -1
}

func (g *generation) String() string {
	return fmt.Sprintf("g: ver[%d], revs %#v\n", g.ver, g.revs)
}

func (a generation) equal(b generation) bool {
	if a.ver != b.ver {
		return false
	}
	if len(a.revs) != len(b.revs) {
		return false
	}

	for i := range a.revs {
		ar, br := a.revs[i], b.revs[i]
		if ar != br {
			return false
		}
	}
	return true
}
