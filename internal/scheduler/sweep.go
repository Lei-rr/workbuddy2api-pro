package scheduler

import (
	"context"
	"log"
	"time"
)

// StartTaskSweep 后台周期性任务补领清扫（独立 ticker goroutine，ctx 取消即停）。
// 与 StartBalanceRefresh 同款结构；interval<=0 不启动（面板关闭开关时 main 不
// 调用即可），运行期可用 SetTaskSweepInterval 热改间隔（下一轮生效）。
//
// 与成长队列（GrowthHook，小时制）的区别：队列执行的是**行为链路**（真实对话、
// 事件上报）成本高；清扫只做 accept/claim 两条幂等读改请求，分钟级跑也几乎零
// 负担，却能补上「达标的任务没人领奖」这条长期缺失的链路（issue #174）。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.sweepInterval.Store(int64(interval))
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: scheduler: 任务补领清扫循环 panic: %v", r)
			}
		}()
		var logged time.Duration
		for {
			cur := time.Duration(s.sweepInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 任务补领清扫每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.schedMu.Lock()
				hook := s.cfg.TaskSweepHook
				s.schedMu.Unlock()
				if hook != nil {
					s.safeHook("task_sweep", hook)
				}
			}
		}
	}()
}

// RunTaskSweepNow 立刻跑一轮任务补领清扫（手动触发/测试用）。
func (s *Scheduler) RunTaskSweepNow() {
	s.schedMu.Lock()
	hook := s.cfg.TaskSweepHook
	s.schedMu.Unlock()
	if hook != nil {
		s.safeHook("task_sweep", hook)
	}
}
