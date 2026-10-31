// A page audit of the newest snapshot, in the spirit of the C peer's
// check-freelist (rlmdb core/audit.rs, interop cpeer_pagewalk.c), for
// tests: every page from 2 to last_pgno must be used exactly once, by a
// tree (branch, leaf and overflow pages of the free DB, the main DB, the
// named DBs and their DUPSORT sub-DBs) or listed in a free DB record; and
// each DB record's depth and page counts must match its tree.

package engine

import (
	"fmt"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

type auditor struct {
	t    *Txn
	used map[uint64]string
}

type auditCounts struct {
	depth                           uint16
	branch, leaf, overflow, entries uint64
}

func (a *auditor) claim(pgno uint64, by string) error {
	if prev, ok := a.used[pgno]; ok {
		return fmt.Errorf("page %d used by %s and %s", pgno, prev, by)
	}
	a.used[pgno] = by
	return nil
}

func (a *auditor) tree(name string, db *format.DB) error {
	if db.Root == format.PInvalid {
		if db.Depth != 0 || db.Entries != 0 {
			return fmt.Errorf("%s: empty tree with depth %d entries %d", name, db.Depth, db.Entries)
		}
		return nil
	}
	var c auditCounts
	if err := a.walk(name, db.Root, 1, &c, false); err != nil {
		return err
	}
	if db.Depth != c.depth || db.BranchPages != c.branch || db.LeafPages != c.leaf ||
		db.OverflowPages != c.overflow || db.Entries != c.entries {
		return fmt.Errorf("%s: record %+v, tree %+v", name, *db, c)
	}
	return nil
}

func (a *auditor) walk(name string, pgno uint64, level uint16, c *auditCounts, sub bool) error {
	if err := a.claim(pgno, "tree "+name); err != nil {
		return err
	}
	page, err := a.t.page(pgno)
	if err != nil {
		return fmt.Errorf("%s: page %d: %w", name, pgno, err)
	}
	c.depth = max(c.depth, level)
	if page.IsBranch() {
		c.branch++
		for i := 0; i < page.NumKeys(); i++ {
			n, ok := page.Node(i)
			if !ok {
				return fmt.Errorf("%s: page %d node %d", name, pgno, i)
			}
			if err := a.walk(name, n.ChildPgno(), level+1, c, sub); err != nil {
				return err
			}
		}
		return nil
	}
	if !page.IsLeaf() {
		return fmt.Errorf("%s: page %d is neither branch nor leaf", name, pgno)
	}
	c.leaf++
	if page.IsLeaf2() || sub {
		c.entries += uint64(page.NumKeys())
		return nil
	}
	for i := 0; i < page.NumKeys(); i++ {
		n, ok := page.Node(i)
		if !ok {
			return fmt.Errorf("%s: page %d node %d", name, pgno, i)
		}
		switch {
		case n.IsBigData():
			ov, _ := n.OverflowPgno()
			op, err := a.t.page(ov)
			if err != nil {
				return err
			}
			cnt := uint64(op.OverflowPages())
			for p := ov; p < ov+cnt; p++ {
				if err := a.claim(p, "overflow "+name); err != nil {
					return err
				}
			}
			c.overflow += cnt
			c.entries++
		case n.IsDupData():
			data, _ := n.InlineData()
			if n.IsSubData() {
				// A sub-DB's pages count towards the main record's.
				sdb, ok := format.ReadDB(data)
				if !ok {
					return fmt.Errorf("%s: bad sub-DB record", name)
				}
				var sc auditCounts
				if err := a.walk(name, sdb.Root, 1, &sc, true); err != nil {
					return err
				}
				if sdb.Entries != sc.entries {
					return fmt.Errorf("%s: sub-DB entries %d, tree %d", name, sdb.Entries, sc.entries)
				}
				c.branch += sc.branch
				c.leaf += sc.leaf
				c.entries += sc.entries
			} else {
				sp, ok := format.NewPage(data, n.L)
				if !ok {
					return fmt.Errorf("%s: bad sub-page", name)
				}
				c.entries += uint64(sp.NumKeys())
			}
		case n.IsSubData() && name == "main":
			data, _ := n.InlineData()
			sdb, ok := format.ReadDB(data)
			if !ok {
				return fmt.Errorf("%s: bad DB record", name)
			}
			k, _ := n.Key()
			if err := a.tree(string(k), &sdb); err != nil {
				return err
			}
			c.entries++
		default:
			c.entries++
		}
	}
	return nil
}

// Audit checks the page usage of the newest snapshot (see the file
// comment). For tests.
func (e *Env) Audit() error {
	t, err := e.BeginTxn(nil, format.ReadOnly)
	if err != nil {
		return err
	}
	defer t.Abort()
	a := &auditor{t: t, used: map[uint64]string{}}
	free, main := t.dbe(format.FreeDBI).db, t.dbe(format.MainDBI).db
	if err := a.tree("free", &free); err != nil {
		return err
	}
	if err := a.tree("main", &main); err != nil {
		return err
	}
	c, err := t.OpenCursor(FreeDBI)
	if err != nil {
		return err
	}
	for {
		k, v, err := c.Get(nil, nil, format.Next)
		if err == format.NotFound {
			break
		}
		if err != nil {
			return err
		}
		id, _ := format.Get64(k, 0)
		ids, ok := format.DecodeIDL(v)
		if !ok {
			return fmt.Errorf("bad free-list record %d", id)
		}
		for _, p := range ids {
			if err := a.claim(p, fmt.Sprintf("free list %d", id)); err != nil {
				return err
			}
		}
	}
	last := t.nextPgno - 1
	for p := uint64(format.NumMetas); p <= last; p++ {
		if _, ok := a.used[p]; !ok {
			return fmt.Errorf("page %d of %d is unaccounted for", p, last)
		}
	}
	for p := range a.used {
		if p > last {
			return fmt.Errorf("page %d past last_pgno %d in use", p, last)
		}
	}
	return nil
}
