package ffmpeg

import (
	"strings"
	"testing"
	"time"
)

func TestParseProgressFallbacksAndFinal(t *testing.T) {
	input := strings.Join([]string{
		"frame=10",
		"out_time_us=1250000",
		"out_time_ms=9999999",
		"out_time=00:00:09.999999",
		"speed=1.2x",
		"unknown=value",
		"progress=continue",
		"out_time_us=N/A",
		"out_time_ms=2500000",
		"out_time=00:00:09.999999",
		"speed=0.75x",
		"progress=continue",
		"out_time_us=bad",
		"out_time_ms=also-bad",
		"out_time=00:00:03.750000",
		"speed=N/A",
		"progress=end",
	}, "\n") + "\n"

	at := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	progress := make(chan ProgressSample, 3)
	if err := parseProgress(strings.NewReader(input), progress, func() time.Time { return at }); err != nil {
		t.Fatalf("parseProgress() error = %v", err)
	}

	want := []ProgressSample{
		{At: at, OutTime: 1250 * time.Millisecond, Speed: 1.2},
		{At: at, OutTime: 2500 * time.Millisecond, Speed: 0.75},
		{At: at, OutTime: 3750 * time.Millisecond, Final: true},
	}
	for index, expected := range want {
		select {
		case got := <-progress:
			if got != expected {
				t.Errorf("sample %d = %#v, want %#v", index, got, expected)
			}
		default:
			t.Fatalf("sample %d was not emitted", index)
		}
	}
}

func TestParseProgressLatestWins(t *testing.T) {
	input := strings.Join([]string{
		"out_time_us=1000000",
		"progress=continue",
		"out_time_us=2000000",
		"progress=continue",
		"out_time_us=3000000",
		"progress=end",
	}, "\n") + "\n"

	progress := make(chan ProgressSample, 1)
	if err := parseProgress(strings.NewReader(input), progress, time.Now); err != nil {
		t.Fatalf("parseProgress() error = %v", err)
	}

	got := <-progress
	if got.OutTime != 3*time.Second || !got.Final {
		t.Fatalf("latest sample = %#v, want final sample at 3s", got)
	}
}

func TestParseClockDurationNegative(t *testing.T) {
	got, ok := parseClockDuration("-00:00:00.500000")
	if !ok || got != -500*time.Millisecond {
		t.Fatalf("parseClockDuration() = (%v, %v), want (-500ms, true)", got, ok)
	}
}
