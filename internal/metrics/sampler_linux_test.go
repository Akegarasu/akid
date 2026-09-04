//go:build linux

package metrics

import "testing"

func TestParseTotalCPUTicksExcludesGuestFields(t *testing.T) {
	total, ok := parseTotalCPUTicks("cpu  1 2 3 4 5 6 7 8 100 200\ncpu0 0\n")
	if !ok || total != 36 {
		t.Fatalf("total=%d ok=%v", total, ok)
	}
}

func TestParseProcessStatHandlesParenthesesAndSpacesInComm(t *testing.T) {
	line := "123 (name with ) paren) S 1 2 3 4 5 6 7 8 9 10 100 20 13 14 15 16 17 18 500 4096 12\n"
	sample, err := parseProcessStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if sample.cpuTicks != 120 || sample.startTime != 500 || sample.rssPages != 12 {
		t.Fatalf("unexpected sample: %#v", sample)
	}
}

func TestParseProcessStatRejectsZombie(t *testing.T) {
	line := "123 (zombie) Z 1 2 3 4 5 6 7 8 9 10 100 20 13 14 15 16 17 18 500 4096 12\n"
	if _, err := parseProcessStat(line); err == nil {
		t.Fatal("expected zombie to be rejected")
	}
}
