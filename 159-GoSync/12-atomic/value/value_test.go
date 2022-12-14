package value

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValue(t *testing.T) {
	var config atomic.Value
	config.Store(loadNewConfig())
	var cond = sync.NewCond(&sync.Mutex{})

	// 设置新的config
	go func() {
		for {
			time.Sleep(time.Duration(5+rand.Int63n(5)) * time.Second)
			config.Store(loadNewConfig())
			cond.Broadcast() // 通知等待着配置已变更
		}
	}()

	go func() {
		for {
			cond.L.Lock()
			cond.Wait()                 // 等待变更信号
			c := config.Load().(Config) // 读取新的配置
			t.Logf("new config: %+v", c)
			cond.L.Unlock()
		}
	}()

	select {}
}
