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
