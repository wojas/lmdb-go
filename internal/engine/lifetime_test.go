package engine

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PowerDNS/lmdb-go/internal/format"
)

// Read transactions begin (reading the meta pages) while SetMapSize
// replaces and releases the map, over and over: every meta read goes
// through a map the reader holds a reference on (review finding: they
// read the current map without one, racing with its unmap; -race reported
// it, and a released map's empty slice made the read panic). Run with
// -race.
func TestMetaReadsDuringSetMapSize(t *testing.T) {
	for _, nolock := range []bool{false, true} {
		t.Run(fmt.Sprintf("nolock=%v", nolock), func(t *testing.T) {
			opts := Options{}
			if nolock {
				opts.Flags = format.NoLock
			}
			env, _ := openTest(t, format.V09, opts)
			putKV(t, env, "k", "v")
			iters := 20000
			if testing.Short() || raceEnabled {
				iters = 2000
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					txn := new(Txn)
					for i := 0; i < iters; i++ {
						if err := env.BeginRead(txn); err != nil {
							errs <- err
							return
						}
						v, err := txn.Get(MainDBI, []byte("k"))
						ok := err == nil && string(v) == "v" // before Abort: v is in the map
						txn.Abort()
						if !ok {
							errs <- fmt.Errorf("Get: %v", err)
							return
						}
					}
				}()
			}
			for i := 0; i < iters/4; i++ {
				if err := env.SetMapSize(64 << 20); err != nil {
					t.Fatal(err)
				}
				if _, err := env.Info(); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
		})
	}
}
