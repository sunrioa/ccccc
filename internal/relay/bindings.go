package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ProjectEndpoint struct {
	DeviceID string `json:"device_id"`
	Project  string `json:"project"`
	Path     string `json:"path"`
}
type ProjectBinding struct {
	ID      string          `json:"id"`
	A       ProjectEndpoint `json:"a"`
	B       ProjectEndpoint `json:"b"`
	Created time.Time       `json:"created"`
}
type bindingRequest struct {
	SourceDevice  string `json:"source_device"`
	SourceProject string `json:"source_project"`
	TargetDevice  string `json:"target_device"`
	TargetProject string `json:"target_project"`
	PreviewID     string `json:"preview_id,omitempty"`
}

func (s *Server) endpoint(device, project string) (ProjectEndpoint, error) {
	d := s.state.Devices[device]
	if d != nil {
		for _, p := range d.Projects {
			if p.Key == project {
				return ProjectEndpoint{device, project, p.Path}, nil
			}
		}
	}
	return ProjectEndpoint{}, fmt.Errorf("设备或项目已不存在，请刷新项目列表")
}
func sameEndpoint(a, b ProjectEndpoint) bool {
	return a.DeviceID == b.DeviceID && a.Project == b.Project
}

// One counterpart per project on each other device. A third device may have
// its own explicit pair; never infer transitive bindings or silently replace.
func (s *Server) bindLocked(a, b ProjectEndpoint) (*ProjectBinding, error) {
	if a.DeviceID == b.DeviceID {
		return nil, fmt.Errorf("请选择另一台设备上的项目")
	}
	for _, old := range s.state.Bindings {
		if sameEndpoint(old.A, b) && sameEndpoint(old.B, a) {
			a, b = b, a
		}
		if sameEndpoint(old.A, a) && sameEndpoint(old.B, b) {
			if old.A.Path != a.Path || old.B.Path != b.Path {
				return nil, fmt.Errorf("已绑定项目的路径发生变化，请解除旧绑定后重新确认")
			}
			return old, nil
		}
		for _, ends := range [][2]ProjectEndpoint{{old.A, old.B}, {old.B, old.A}} {
			if sameEndpoint(ends[0], a) && ends[1].DeviceID == b.DeviceID || sameEndpoint(ends[0], b) && ends[1].DeviceID == a.DeviceID {
				return nil, fmt.Errorf("该项目在对方设备上已有绑定，请先解除旧绑定")
			}
		}
	}
	if len(s.state.Bindings) >= 1000 {
		return nil, fmt.Errorf("项目绑定已达 1000 条，请先清理旧绑定")
	}
	p := &ProjectBinding{ID: id(), A: a, B: b, Created: time.Now().UTC()}
	s.state.Bindings[p.ID] = p
	if err := s.save(); err != nil {
		delete(s.state.Bindings, p.ID)
		return nil, err
	}
	return p, nil
}

// Called under the server state lock, behind administrator authentication.
func (s *Server) bindingAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/bindings" && r.Method == "POST" {
		var req bindingRequest
		if err := body(w, r, &req); err != nil {
			fail(w, 400, err)
			return
		}
		var a, b ProjectEndpoint
		var err error
		if req.PreviewID != "" {
			j := s.state.Jobs[req.PreviewID]
			if j == nil || j.Kind != "preview" || j.Status != "done" {
				fail(w, 400, "请先完成恢复预览")
				return
			}
			snapshot := s.state.Snapshots[j.SnapshotID]
			var plan Plan
			if snapshot == nil || s.state.Devices[snapshot.DeviceID] == nil || json.Unmarshal(j.Result, &plan) != nil || !plan.CanApply || plan.Target == "" {
				fail(w, 400, "该预览无法创建绑定")
				return
			}
			a = ProjectEndpoint{snapshot.DeviceID, snapshot.Project, snapshot.SourcePath}
			b = ProjectEndpoint{j.DeviceID, j.Project, plan.Target}
		} else {
			a, err = s.endpoint(req.SourceDevice, req.SourceProject)
			if err == nil {
				b, err = s.endpoint(req.TargetDevice, req.TargetProject)
			}
			if err != nil {
				fail(w, 400, err)
				return
			}
		}
		binding, err := s.bindLocked(a, b)
		if err != nil {
			fail(w, 409, err)
			return
		}
		reply(w, binding)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/bindings/") && r.Method == "DELETE" {
		key := strings.TrimPrefix(r.URL.Path, "/api/bindings/")
		old := s.state.Bindings[key]
		if old == nil {
			fail(w, 404, "绑定不存在")
			return
		}
		delete(s.state.Bindings, key)
		if err := s.save(); err != nil {
			s.state.Bindings[key] = old
			fail(w, 500, err)
			return
		}
		reply(w, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, "not found")
}
