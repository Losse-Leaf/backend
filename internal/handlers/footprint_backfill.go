package handlers

import (
	"log"
	"sync"
	"time"

	"loose-leaf/internal/db"
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

			// 后台回填跨越多次查询与事务，必须与整库恢复互斥：
			// 否则恢复替换数据库时它可能把旧库数据写进新库，或撞上被关闭的连接池。
			db.LockRead()
			defer db.UnlockRead()

			start := time.Now()
			log.Println("[Footprint Backfill] 启动回填开始（后台执行，不阻塞 HTTP 监听）")
			EnsureEntryFootprintsSynced()
			EnsureFootprintTransactionsLinked()
			log.Printf("[Footprint Backfill] 启动回填结束，总耗时 %s", time.Since(start).Round(time.Millisecond))
		}()
	})
}

var footprintBackfillOnce sync.Once
