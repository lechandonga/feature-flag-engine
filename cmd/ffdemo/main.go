// ffdemo 演示本地文件配置 + 后台刷新 + 评估与归因的最小用法。
// 运行期间编辑 configs/flags-v2.json 即可模拟配置下发。
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lechandonga/feature-flag-engine/engine"
)

type logSink struct{}

func (logSink) Send(a engine.Attribution) error {
	log.Printf("[attribution] flag=%s user=%s variation=%s source=%s rule=%s v=%d",
		a.FlagKey, a.UserID, a.Variation, a.Source, a.HitRuleID, a.ConfigVersion)
	return nil
}

func main() {
	reporter := engine.NewChannelReporter(logSink{}, engine.ReporterOptions{
		QueueSize: 64,
		DedupeTTL: 10 * time.Second,
	})
	e := engine.New(engine.WithReporter(reporter))

	ctx := context.Background()
	if err := e.StartRefresher(ctx, &engine.FileSource{Path: "configs/flags-v2.json"}, 3*time.Second); err != nil {
		log.Fatalf("initial load failed: %v", err)
	}
	defer e.Close()

	users := []engine.User{
		{ID: "user-001", Attributes: map[string]string{"tier": "staff"}},
		{ID: "user-002"},
		{ID: "user-003"},
	}
	for i := 0; i < 3; i++ {
		fmt.Println("--- evaluation round", i, "config v", e.CurrentVersion(), "stale:", e.IsStale())
		for _, u := range users {
			r := e.Evaluate("checkout_redesign", u)
			fmt.Printf("user=%-9s status=%-20s variation=%-9s source=%-19s rule=%s\n",
				u.ID, r.Status, r.Variation, r.Source, r.HitRuleID)
		}
		time.Sleep(5 * time.Second)
	}
}
