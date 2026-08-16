package ffmpeg

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// ProgressSample is one complete record from FFmpeg's -progress protocol.
type ProgressSample struct {
	At      time.Time
	OutTime time.Duration
	Speed   float64
	Final   bool
}

type progressRecord struct {
	outTimeUS string
	outTimeMS string
	outTime   string
	speed     string
}

func parseProgress(r io.Reader, out chan ProgressSample, now func() time.Time) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 64*1024)

	var record progressRecord
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "out_time_us":
			record.outTimeUS = value
		case "out_time_ms":
			record.outTimeMS = value
		case "out_time":
			record.outTime = value
		case "speed":
			record.speed = value
		case "progress":
			final := value == "end"
			if value == "continue" || final {
				publishLatest(out, ProgressSample{
					At:      now(),
					OutTime: record.duration(),
					Speed:   parseSpeed(record.speed),
					Final:   final,
				})
			}
			record = progressRecord{}
		}
	}
	return scanner.Err()
}

func (r progressRecord) duration() time.Duration {
	if d, ok := parseProtocolMicros(r.outTimeUS); ok {
		return d
	}
	// Despite its name, out_time_ms is also expressed in microseconds by
	// FFmpeg's progress protocol.
	if d, ok := parseProtocolMicros(r.outTimeMS); ok {
		return d
	}
	if d, ok := parseClockDuration(r.outTime); ok {
		return d
	}
	return 0
}

func parseProtocolMicros(value string) (time.Duration, bool) {
	micros, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, false
	}

	const nanosPerMicro = int64(time.Microsecond)
	const maxInt64 = int64(^uint64(0) >> 1)
	const minInt64 = -maxInt64 - 1
	if micros > maxInt64/nanosPerMicro || micros < minInt64/nanosPerMicro {
		return 0, false
	}
	return time.Duration(micros * nanosPerMicro), true
}

func parseClockDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return 0, false
	}

	sign := 1.0
	if value[0] == '-' {
		sign = -1
		value = value[1:]
	} else if value[0] == '+' {
		value = value[1:]
	}

	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || minutes >= 60 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || seconds < 0 || seconds >= 60 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, false
	}

	totalNanos := sign * ((float64(hours)*3600 + float64(minutes)*60 + seconds) * float64(time.Second))
	if totalNanos > float64(math.MaxInt64) || totalNanos < float64(math.MinInt64) {
		return 0, false
	}
	return time.Duration(totalNanos), true
}

func parseSpeed(value string) float64 {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "x")
	speed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(speed) || math.IsInf(speed, 0) {
		return 0
	}
	return speed
}

func publishLatest(ch chan ProgressSample, sample ProgressSample) {
	select {
	case ch <- sample:
		return
	default:
	}

	select {
	case <-ch:
	default:
	}

	select {
	case ch <- sample:
	default:
	}
}
