package tui

import (
	"testing"

	"akid/internal/model"
)

func TestProcessListLiveSearchCanBeCanceled(t *testing.T) {
	list := newProcessList()
	list.Set([]model.ProcessInfo{processInfo("api", "1"), processInfo("worker", "2")})
	list.query = "api"
	list.applyFilter("")
	list.BeginSearch()
	list.UpdateSearch("worker")
	if len(list.visible) != 1 || list.visible[0].Config.Name != "worker" {
		t.Fatalf("live search result: %#v", list.visible)
	}
	list.EndSearch(false)
	if list.query != "api" || len(list.visible) != 1 || list.visible[0].Config.Name != "api" {
		t.Fatalf("cancel did not restore filter: query=%q visible=%#v", list.query, list.visible)
	}
}

func TestProcessListRejectsStaleActionsAndDeletedRows(t *testing.T) {
	list := newProcessList()
	info := processInfo("api", "1")
	info.Epoch, info.Revision = "daemon-a", 1
	list.ApplySnapshot(model.ProcessSnapshot{Epoch: info.Epoch, Revision: 1, Processes: []model.ProcessInfo{info}})
	stopped := info
	stopped.Revision, stopped.Runtime.Status = 3, model.StatusStopped
	if !list.ApplyChange(stopped, false) {
		t.Fatal("new event rejected")
	}
	info.Revision = 2
	if list.ApplyChange(info, false) || list.all[0].Runtime.Status != model.StatusStopped {
		t.Fatal("old response replaced newer event")
	}
	deleted := stopped
	deleted.Revision = 4
	list.ApplyChange(deleted, true)
	if list.ApplyChange(stopped, false) || len(list.all) != 0 {
		t.Fatal("late response resurrected deleted process")
	}
	list.ApplySnapshot(model.ProcessSnapshot{Epoch: info.Epoch, Revision: 2, Processes: []model.ProcessInfo{info}})
	if len(list.all) != 0 {
		t.Fatal("stale snapshot resurrected deleted process")
	}
	info.Epoch, info.Revision = "daemon-b", 1
	list.ApplySnapshot(model.ProcessSnapshot{Epoch: info.Epoch, Revision: 1, Processes: []model.ProcessInfo{info}})
	if list.ApplyChange(deleted, true) || len(list.all) != 1 {
		t.Fatal("old daemon response affected new daemon state")
	}
}

func TestSnapshotPreservesNewerActionResponse(t *testing.T) {
	list := newProcessList()
	info := processInfo("api", "1")
	info.Epoch, info.Revision = "daemon", 1
	list.ApplySnapshot(model.ProcessSnapshot{Epoch: "daemon", Revision: 1, Processes: []model.ProcessInfo{info}})
	newer := info
	newer.Revision, newer.Runtime.Status = 5, model.StatusStopped
	list.ApplyChange(newer, false)
	list.ApplySnapshot(model.ProcessSnapshot{Epoch: "daemon", Revision: 3, Processes: []model.ProcessInfo{info}})
	if len(list.all) != 1 || list.all[0].Revision != 5 {
		t.Fatal("snapshot overwrote later action")
	}
}

func TestProcessListSortFilterAndPreserveSelection(t *testing.T) {
	list := newProcessList()
	list.Set([]model.ProcessInfo{
		processInfo("worker", "2"),
		processInfo("api", "1"),
		processInfo("scheduler", "3"),
	})
	if got := list.visible[0].Config.Name; got != "api" {
		t.Fatalf("list not sorted: first=%q", got)
	}
	list.selected = 1 // worker
	list.query = "work"
	list.applyFilter(list.SelectedID())
	if len(list.visible) != 1 || list.visible[0].Config.ID != "2" || list.selected != 0 {
		t.Fatalf("unexpected filtered selection: %#v selected=%d", list.visible, list.selected)
	}

	updated := processInfo("worker", "2")
	updated.Runtime.PID = 999
	list.Upsert(updated)
	selected, ok := list.Selected()
	if !ok || selected.Runtime.PID != 999 {
		t.Fatalf("upsert did not preserve/update selection: %#v", selected)
	}
}
