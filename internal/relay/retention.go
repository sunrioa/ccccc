package relay

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Keep the small metadata record: existing local previews do not depend on
// a retained server archive, and can still be applied after eviction.
func (s *Server) purgeLocked(b *Snapshot) error {
	if err := os.Remove(filepath.Join(s.Dir, "bundles", b.ID+".zip")); err != nil && !os.IsNotExist(err) {
		return err
	}
	b.Purged = true
	return s.save()
}
func (s *Server) cleanupLocked(now time.Time) error {
	for _, b := range s.state.Snapshots {
		if !b.Temporary || b.Purged {
			continue
		}
		consumed, busy := false, false
		expired := !b.Expires.IsZero() && !now.Before(b.Expires)
		for _, j := range s.state.Jobs {
			if j.SnapshotID != b.ID {
				continue
			}
			if j.Kind == "preview" && j.Status == "done" {
				var p Plan
				if json.Unmarshal(j.Result, &p) == nil && p.CanApply {
					consumed = true
				}
			}
			if j.Kind == "preview" && (j.Status == "running" || j.Status == "queued") {
				if expired && j.Status == "queued" {
					j.Status = "failed"
					j.Error = "临时中转包已超过 24 小时，请在来源设备重新创建"
					j.Updated = now
				} else {
					busy = true
				}
			}
		}
		if !busy && (consumed || expired) {
			if err := s.purgeLocked(b); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Server) Cleanup(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupLocked(now)
}
func (s *Server) RunCleanup(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if err := s.Cleanup(time.Now()); err != nil {
			log.Printf("临时包清理稍后重试：%v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
