package apireadcache

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func BenchmarkAPIReadCachePersistPointUpdate(b *testing.B) {
	for _, count := range []int{1, 512} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			cache := &apiReadCache{schedulerDir: b.TempDir()}
			body := []byte(strings.Repeat("response-body", 800))
			e := apiReadCacheEntry{Body: body, ETag: `"initial"`, Stored: time.Now().Unix()}
			for i := range count {
				cache.store(fmt.Sprint(i), e)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				e.ETag = fmt.Sprintf(`"refresh-%d"`, i)
				cache.store("0", e)
			}
		})
	}
}
