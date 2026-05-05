package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/x/oracle/daemon"
)

func TestCacheSetGet(t *testing.T) {
	c := daemon.NewCache()
	now := time.Now()
	c.Set(1, 100, now)
	got, ok := c.Get(1)
	require.True(t, ok)
	require.EqualValues(t, 100, got.Price)
	require.Equal(t, now, got.UpdatedAt)
}

func TestCacheRejectsZero(t *testing.T) {
	c := daemon.NewCache()
	c.Set(1, 0, time.Now())
	_, ok := c.Get(1)
	require.False(t, ok)
}

func TestCacheSnapshotAgeFilter(t *testing.T) {
	c := daemon.NewCache()
	now := time.Date(2026, 5, 5, 19, 0, 0, 0, time.UTC)
	c.Set(1, 100, now.Add(-30*time.Second))
	c.Set(2, 200, now.Add(-1*time.Second))
	out := c.Snapshot(now, 5*time.Second)
	require.Len(t, out, 1)
	require.EqualValues(t, 2, out[0].MarketIndex)
}

func TestCacheConcurrent(t *testing.T) {
	c := daemon.NewCache()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Set(uint32(i), uint32(j+1), time.Now())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = c.Get(uint32(i))
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, c.Size(), 32)
}
