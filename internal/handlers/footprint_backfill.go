package handlers

import (
	"log"
	"sync"
	"time"
)

// StartFootprintBackfill 在 HTTP server 启动后异步执行启动回填。
//
// 关键（BE-5）：原先 main 在 ListenAndServe 之前同步跑两个全量回填，
// 数据量大时端口迟迟不监听，健康检查与客户端全部超时。
// 现在改为 goroutine + sync.Once：HTTP 先可用，回填在后台分批进行；
// 重复调用 StartFootprintBackfill 不会叠加任务。
func StartFootprintBackfill() {
	footprintBackfillOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Footprint Backfill] 启动回填异常崩溃: %v", r)
				}
			}()

			start := time.Now()
			log.Println("[Footprint Backfill] 启动回填开始（后台执行，不阻塞 HTTP 监听）")
			EnsureEntryFootprintsSynced()
			EnsureFootprintTransactionsLinked()
			log.Printf("[Footprint Backfill] 启动回填结束，总耗时 %s", time.Since(start).Round(time.Millisecond))
		}()
	})
}

var footprintBackfillOnce sync.Once
