package ffmpeg

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-zeromq/zmq4"
)

// AnimateOverlay applies the intro transition schedule through ZMQ. The text
// files remain the source of truth, so updates do not require restarting FFmpeg.
func (s *Session) AnimateOverlay() error {
	s.mu.RLock()
	endpoint := s.overlayEndpoint
	startedAt := s.overlayStartedAt
	s.mu.RUnlock()
	if endpoint == "" {
		return nil
	}

	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()

	t := time.Since(startedAt).Seconds()
	base := strconv.FormatFloat(t, 'f', 3, 64)
	quality := overlayCommand("quality", base, 24)
	languages := overlayCommand("languages", base, 58)
	metadata := overlayCommand("metadata", base, 570)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := sendOverlayCommand(endpoint, quality+"\n"+languages+"\n"+metadata); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("overlay animation command failed after retries: %w", lastErr)
}

func overlayCommand(name, base string, y int) string {
	_ = base
	return fmt.Sprintf("%s reinit x=24:y=%d:alpha='if(lt(t,5),0,if(lt(t,5.8),(t-5)/0.8,1))'", name, y)
}

func sendOverlayCommand(endpoint, commands string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	socket := zmq4.NewReq(ctx)
	defer socket.Close()
	if err := socket.Dial(endpoint); err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	for _, command := range strings.Split(commands, "\n") {
		if err := socket.Send(zmq4.NewMsgString(command)); err != nil {
			return fmt.Errorf("send %q: %w", command, err)
		}
		response, err := socket.Recv()
		if err != nil {
			return fmt.Errorf("receive response for %q: %w", command, err)
		}
		text := strings.TrimSpace(response.String())
		upper := strings.ToUpper(text)
		if strings.HasPrefix(upper, "ERROR") || strings.Contains(upper, "FAIL") {
			return fmt.Errorf("ffmpeg rejected %q: %s", command, text)
		}
	}
	return nil
}
