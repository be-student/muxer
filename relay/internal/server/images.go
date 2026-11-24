package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/gantoine/muxer/relay/internal/chat"
	"github.com/gantoine/muxer/relay/internal/claude"
	"github.com/gantoine/muxer/relay/internal/cmux"
	"github.com/gantoine/muxer/relay/internal/pi"
)

var errImagesUnsupported = unsupported("receive images")

var imageFormats = map[string]bool{"png": true, "jpeg": true, "jpg": true, "webp": true, "gif": true}

const maxAttachedImageRefs = 1024

// Only files announced in this attachment's chat can be fetched. Retain their
// identity so replacing a file or retargeting a parent symlink cannot substitute
// a different file after it was announced. Old references expire after 1,024
// newer files; a new chat snapshot refreshes the visible history.
type attachedImages struct {
	mu    sync.RWMutex
	files map[string]os.FileInfo
	order []string
}

func (a *attachedImages) remember(entries []chat.Entry, reset bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if reset || a.files == nil {
		a.files = make(map[string]os.FileInfo)
		a.order = nil
	}
	remember := func(path string) {
		if filepath.Clean(path) != path || !chat.IsAttachedImage(path) {
			return
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		if _, exists := a.files[path]; !exists {
			if len(a.order) == maxAttachedImageRefs {
				delete(a.files, a.order[0])
				a.order = a.order[1:]
			}
			a.order = append(a.order, path)
		}
		a.files[path] = info
	}
	for _, entry := range entries {
		for _, path := range entry.Images {
			remember(path)
		}
		if entry.Tool != nil {
			for _, path := range entry.Tool.Images {
				remember(path)
			}
		}
	}
}

func validImages(images []cmux.ChatImage) error {
	if len(images) == 0 || len(images) > maxImages {
		return invalid(fmt.Sprintf("Send 1 to %d images at a time.", maxImages))
	}
	for _, image := range images {
		if !imageFormats[image.Format] {
			return invalid("Images must be PNG, JPEG, WebP or GIF.")
		}
		// Go's standard decoder permits CR/LF-wrapped base64; do not reject
		// those existing inputs when checking the encoded length.
		encodedSize := len(image.Data) - strings.Count(image.Data, "\r") - strings.Count(image.Data, "\n")
		if encodedSize > base64.StdEncoding.EncodedLen(maxImageBytes) {
			return invalid(fmt.Sprintf("Each image can be up to %d MB.", maxImageBytes>>20))
		}
		// Validate without allocating a second copy of an untrusted payload.
		reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(image.Data))
		size, err := io.Copy(io.Discard, io.LimitReader(reader, maxImageBytes+1))
		if size > maxImageBytes {
			return invalid(fmt.Sprintf("Each image can be up to %d MB.", maxImageBytes>>20))
		}
		if err != nil || size == 0 {
			return invalid("An image wasn't valid base64 data.")
		}
	}
	return nil
}

func (s *session) sendImages(ctx context.Context, current *attachment, text string, images []cmux.ChatImage) error {
	return guard(s.server, cmux.MethodChatSend, errImagesUnsupported, func() error {
		agent, err := s.server.chatAgent(ctx, current.workspaceID, current.surfaceID)
		if err != nil {
			return err
		}
		if agent.SessionID == "" {
			return &requestError{Code: "no_agent", Message: "Images can only go to Claude, Codex or Pi, and none of them is running in this terminal."}
		}
		return s.server.cmux.SendChat(ctx, agent.SessionID, text, images)
	})
}

func (a *attachedImages) read(path string) (cmux.ChatImage, error) {
	if filepath.Clean(path) != path || !chat.IsAttachedImage(path) {
		return cmux.ChatImage{}, invalid("That isn't an image sent to the agent.")
	}
	gone := &requestError{Code: "not_found", Message: "That image is no longer on the computer."}
	a.mu.RLock()
	expected := a.files[path]
	a.mu.RUnlock()
	if expected == nil {
		return cmux.ChatImage{}, gone
	}
	// No-follow rejects a substituted leaf symlink, and non-blocking prevents
	// a substituted FIFO from blocking before its type can be checked.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return cmux.ChatImage{}, gone
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return cmux.ChatImage{}, gone
	}
	if info.Size() > maxImageBytes {
		return cmux.ChatImage{}, invalid(fmt.Sprintf("Images can be up to %d MB.", maxImageBytes>>20))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil {
		return cmux.ChatImage{}, gone
	}
	if len(data) > maxImageBytes {
		return cmux.ChatImage{}, invalid(fmt.Sprintf("Images can be up to %d MB.", maxImageBytes>>20))
	}
	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	return cmux.ChatImage{Data: base64.StdEncoding.EncodeToString(data), Format: format}, nil
}

func (s *session) transcriptImage(ctx context.Context, current *attachment, ref string) (cmux.ChatImage, error) {
	if !claude.IsImageRef(ref) {
		return cmux.ChatImage{}, invalid("That isn't an image from this conversation.")
	}
	gone := &requestError{Code: "not_found", Message: "That image is no longer in the conversation."}
	agent, err := s.server.chatAgent(ctx, current.workspaceID, current.surfaceID)
	if err != nil {
		return cmux.ChatImage{}, err
	}
	path, ok := s.server.transcriptPath(agent)
	if !ok {
		return cmux.ChatImage{}, gone
	}
	switch agent.Kind {
	case agentKindClaude:
		image, err := claude.ReadImage(path, ref)
		if err != nil {
			return cmux.ChatImage{}, gone
		}
		result := cmux.ChatImage{Data: image.Data, Format: strings.TrimPrefix(image.MediaType, "image/")}
		return result, validImages([]cmux.ChatImage{result})
	case agentKindPi:
		image, err := pi.ReadImage(path, ref)
		if err != nil {
			return cmux.ChatImage{}, gone
		}
		result := cmux.ChatImage{Data: image.Data, Format: strings.TrimPrefix(image.MediaType, "image/")}
		return result, validImages([]cmux.ChatImage{result})
	}
	return cmux.ChatImage{}, gone
}
