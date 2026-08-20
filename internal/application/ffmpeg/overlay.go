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
	poster := posterOverlayCommand("poster")
	posterBorder := posterOverlayCommand("poster_border")
	for attempt := 0; attempt < 5; attempt++ {
		if err := sendOverlayCommand(endpoint, quality+"\n"+languages+"\n"+metadata+"\n"+poster+"\n"+posterBorder); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("overlay animation command failed")
}

func posterOverlayCommand(name string) string {
	// Keep the poster hidden until five seconds, then slide it in through ZMQ.
	return fmt.Sprintf("%s x if(lt(t,5),1280,max(947,1280-(t-5)*416.25))", name)
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
		return err
	}
	for _, command := range strings.Split(commands, "\n") {
		if err := socket.Send(zmq4.NewMsgString(command)); err != nil {
			return err
		}
		if _, err := socket.Recv(); err != nil {
			return err
		}
	}
	return nil
}
