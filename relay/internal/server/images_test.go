package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket/wsjson"
	"golang.org/x/sys/unix"

	"github.com/gantoine/muxer/relay/internal/chat"
	"github.com/gantoine/muxer/relay/internal/cmux"
)

func TestPhoneCanSendImagesToClaude(t *testing.T) {
	relay := startRelay(t)
	ctx, conn := relay.connect(t)
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "hello", "id": "1"})
	resultFor(t, ctx, conn, "1")

	photo := base64.StdEncoding.EncodeToString([]byte("jpeg bytes"))
	images := []map[string]string{{"data_b64": photo, "format": "jpeg"}}

	_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": "2", "images": images})
	if result := resultFor(t, ctx, conn, "2"); errorCode(result) != "not_attached" {
		t.Fatalf("images need an attached terminal: %v", result)
	}

	_ = wsjson.Write(ctx, conn, map[string]any{"type": "attach", "workspace_id": "w1", "surface_id": "t1"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": "3", "images": images})
	if result := resultFor(t, ctx, conn, "3"); errorCode(result) != "no_agent" {
		t.Fatalf("a terminal without Claude must refuse images: %v", result)
	}

	relay.cmux.setAgent(cmux.AgentSession{Kind: "claude", SessionID: "s1"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": "4", "text": "what's wrong here?", "images": images})
	if result := resultFor(t, ctx, conn, "4"); result["ok"] != true {
		t.Fatalf("send_images failed: %v", result)
	}
	relay.cmux.mu.Lock()
	chats := relay.cmux.chats
	relay.cmux.mu.Unlock()
	if len(chats) != 1 || chats[0].sessionID != "s1" || chats[0].text != "what's wrong here?" || chats[0].images[0].Data != photo || chats[0].images[0].Format != "jpeg" {
		t.Fatalf("the images must reach the Claude session: %+v", chats)
	}

	relay.cmux.setAgent(cmux.AgentSession{Kind: "codex", SessionID: "thread-1"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": "4b", "images": images})
	if result := resultFor(t, ctx, conn, "4b"); result["ok"] != true {
		t.Fatalf("send_images to Codex failed: %v", result)
	}
	relay.cmux.mu.Lock()
	chats = relay.cmux.chats
	relay.cmux.mu.Unlock()
	if len(chats) != 2 || chats[1].sessionID != "thread-1" {
		t.Fatalf("the images must reach the Codex thread: %+v", chats)
	}

	for id, bad := range map[string][]map[string]string{
		"5": nil,
		"6": {{"data_b64": photo, "format": "bmp"}},
		"7": {{"data_b64": "not base64!", "format": "png"}},
		"8": {{"data_b64": base64.StdEncoding.EncodeToString(make([]byte, maxImageBytes+1)), "format": "png"}},
		"9": {images[0], images[0], images[0], images[0], images[0]},
	} {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": id, "images": bad})
		if result := resultFor(t, ctx, conn, id); errorCode(result) != "invalid_params" {
			t.Fatalf("request %s must be refused: %v", id, result)
		}
	}

	relay.cmux.mu.Lock()
	relay.cmux.chatErr = &cmux.RPCError{Code: "method_not_found", Message: "Unknown method"}
	relay.cmux.mu.Unlock()
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "send_images", "id": "10", "images": images})
	if result := resultFor(t, ctx, conn, "10"); errorCode(result) != "unsupported" {
		t.Fatalf("an older cmux must be reported as unsupported: %v", result)
	}
}

func TestComputerNameComesFromCmuxUnlessSetByHand(t *testing.T) {
	relay := startRelay(t)
	relay.server.options.MacName = ""
	relay.server.options.FallbackMacName = "macbook"

	name := func() string {
		response, err := http.Get(relay.url + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var health struct {
			MacName string `json:"mac_name"`
		}
		_ = json.NewDecoder(response.Body).Decode(&health)
		return health.MacName
	}

	relay.cmux.mu.Lock()
	relay.cmux.hostName = "Studio"
	relay.cmux.mu.Unlock()
	if got := name(); got != "Studio" {
		t.Fatalf("name = %q, want cmux's name", got)
	}

	relay.cmux.mu.Lock()
	relay.cmux.hostNameErr = &cmux.RPCError{Code: "method_not_found", Message: "Unknown method"}
	relay.cmux.mu.Unlock()
	if got := name(); got != "macbook" {
		t.Fatalf("name = %q, want the fallback when cmux can't say", got)
	}

	relay.server.options.MacName = "Work laptop"
	if got := name(); got != "Work laptop" {
		t.Fatalf("name = %q, want the name given with --name", got)
	}
}

func TestPhoneCanFetchImagesFromTheChat(t *testing.T) {
	relay := startRelay(t)
	ctx, conn := relay.connect(t)
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "hello", "id": "1"})
	resultFor(t, ctx, conn, "1")

	dir := t.TempDir()
	path := filepath.Join(dir, "clipboard-2026-10-17-084701-6FFF738B.jpeg")
	if err := os.WriteFile(path, []byte("jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "before_attach", "image": path})
	if result := resultFor(t, ctx, conn, "before_attach"); errorCode(result) != "not_attached" {
		t.Fatalf("clipboard images need an attached conversation: %v", result)
	}
	transcript := filepath.Join(relay.claudeDir, "projects", "-Users-me-app", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": path + " what is this?"}})
	if err := os.WriteFile(transcript, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	relay.cmux.setAgent(cmux.AgentSession{Kind: "claude", SessionID: "s1", Cwd: "/Users/me/app"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "attach", "workspace_id": "w1", "surface_id": "t1"})
	readUntil(t, ctx, conn, "chat")
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "2", "image": path})
	result := resultFor(t, ctx, conn, "2")
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["format"] != "jpeg" || data["data_b64"] != base64.StdEncoding.EncodeToString([]byte("jpeg bytes")) {
		t.Fatalf("chat_image = %v", result)
	}

	secret := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(secret, []byte("private"), 0o600)
	unannounced := filepath.Join(dir, "clipboard-2026-10-17-084701-00000001.jpeg")
	_ = os.WriteFile(unannounced, []byte("not in this conversation"), 0o600)
	for id, other := range map[string]string{
		"3": secret,
		"4": dir + "/x/../clipboard-2026-10-17-084701-6FFF738B.jpeg",
		"5": filepath.Join(dir, "clipboard-2026-10-17-084701-00000000.jpeg"),
		"6": unannounced,
	} {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": id, "image": other})
		if result := resultFor(t, ctx, conn, id); result["ok"] == true {
			t.Errorf("chat_image served %s: %v", other, result)
		}
	}
	relay.cmux.setAgent(cmux.AgentSession{})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "attach", "id": "switch", "workspace_id": "w1", "surface_id": "t2"})
	resultFor(t, ctx, conn, "switch")
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "old_attachment", "image": path})
	if result := resultFor(t, ctx, conn, "old_attachment"); result["ok"] == true {
		t.Fatalf("another attachment must not inherit image access: %v", result)
	}
}

func TestAttachedImageIdentityAndBounds(t *testing.T) {
	for _, change := range []string{"unchanged", "replace", "leaf_symlink", "parent_symlink", "fifo", "oversize"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			realDir := filepath.Join(dir, "real")
			otherDir := filepath.Join(dir, "other")
			for _, path := range []string{realDir, otherDir} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			alias := filepath.Join(dir, "alias")
			if err := os.Symlink(realDir, alias); err != nil {
				t.Fatal(err)
			}
			name := "clipboard-2026-10-17-084701-6FFF738B.jpeg"
			path := filepath.Join(alias, name)
			if err := os.WriteFile(path, []byte("announced"), 0o600); err != nil {
				t.Fatal(err)
			}
			var allowed attachedImages
			allowed.remember([]chat.Entry{{Images: []string{path}}}, true)
			switch change {
			case "replace":
				// Keep the original inode alive so this proves an identity change.
				if err := os.Rename(path, filepath.Join(realDir, "old")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("different"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "leaf_symlink":
				if err := os.Rename(path, filepath.Join(realDir, "old")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("old", path); err != nil {
					t.Fatal(err)
				}
			case "parent_symlink":
				if err := os.WriteFile(filepath.Join(otherDir, name), []byte("different"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(otherDir, alias); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(path, maxImageBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			image, err := allowed.read(path)
			if change == "unchanged" {
				if err != nil || image.Data != base64.StdEncoding.EncodeToString([]byte("announced")) {
					t.Fatalf("an existing parent alias must work: image=%+v, err=%v", image, err)
				}
			} else if err == nil {
				t.Fatalf("served %s file", change)
			}
		})
	}
}

func TestAttachedImageReferencesExpireAndReset(t *testing.T) {
	dir := t.TempDir()
	var allowed attachedImages
	var first, last string
	for i := 0; i <= maxAttachedImageRefs; i++ {
		path := filepath.Join(dir, fmt.Sprintf("clipboard-2026-10-17-084701-%08x.png", i))
		if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
		allowed.remember([]chat.Entry{{Tool: &chat.Tool{Images: []string{path}}}}, false)
		if i == 0 {
			first = path
		}
		last = path
	}
	if _, err := allowed.read(first); err == nil {
		t.Fatal("old reference was not evicted")
	}
	if _, err := allowed.read(last); err != nil {
		t.Fatalf("new reference lost: %v", err)
	}
	allowed.remember([]chat.Entry{{Images: []string{first}}}, true)
	if _, err := allowed.read(first); err != nil {
		t.Fatalf("snapshot did not refresh history: %v", err)
	}
	if _, err := allowed.read(last); err == nil {
		t.Fatal("reset kept a prior conversation's reference")
	}
}

func TestImageValidationBoundsAndWrappedBase64(t *testing.T) {
	maximum := base64.StdEncoding.EncodeToString(make([]byte, maxImageBytes))
	images := make([]cmux.ChatImage, maxImages)
	for i := range images {
		images[i] = cmux.ChatImage{Data: maximum, Format: "png"}
	}
	if err := validImages(images); err != nil {
		t.Fatalf("four maximum-size images must still work: %v", err)
	}
	for name, data := range map[string]string{
		"maximum": maximum,
		"wrapped": maximum[:80] + "\r\n" + maximum[80:] + "\n",
	} {
		if err := validImages([]cmux.ChatImage{{Data: data, Format: "png"}}); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, data := range map[string]string{
		"one_byte_over":      base64.StdEncoding.EncodeToString(make([]byte, maxImageBytes+1)),
		"encoded_over":       strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)+4),
		"invalid_tail":       base64.StdEncoding.EncodeToString([]byte("image")) + "!",
		"data_after_padding": "YQ==YQ==",
		"missing_padding":    "YQ",
		"empty":              "\r\n",
	} {
		if err := validImages([]cmux.ChatImage{{Data: data, Format: "png"}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPhoneCanFetchImagesClaudeLookedAt(t *testing.T) {
	relay := startRelay(t)
	ctx, conn := relay.connect(t)
	transcript := filepath.Join(relay.claudeDir, "projects", "-Users-me-app", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	screenshot := base64.StdEncoding.EncodeToString([]byte("png bytes"))
	lines := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/Users/me/app/shot.png"}}]}}` + "\n" +
		`{"type":"user","uuid":"b2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + screenshot + `"}}]}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "hello", "id": "1"})
	resultFor(t, ctx, conn, "1")
	relay.cmux.setAgent(cmux.AgentSession{Kind: "claude", SessionID: "s1", Cwd: "/Users/me/app"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "attach", "workspace_id": "w1", "surface_id": "t1"})

	chat := readUntil(t, ctx, conn, "chat")
	entries := chat["entries"].([]any)
	tool := entries[0].(map[string]any)["tool"].(map[string]any)
	images, _ := tool["images"].([]any)
	if len(images) != 1 || images[0] != "b2:0:0" {
		t.Fatalf("tool entry = %v", tool)
	}

	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "2", "image": "b2:0:0"})
	result := resultFor(t, ctx, conn, "2")
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["format"] != "png" || data["data_b64"] != screenshot {
		t.Fatalf("chat_image = %v", result)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "3", "image": "a1:0"})
	if result := resultFor(t, ctx, conn, "3"); errorCode(result) != "not_found" {
		t.Fatalf("a block that isn't an image = %v", result)
	}
}

func TestPhoneCanFetchImagesPastedIntoPi(t *testing.T) {
	relay := startRelay(t)
	ctx, conn := relay.connect(t)
	session := filepath.Join(relay.piDir, "sessions", "--Users-me-app--", "2026-10-20T11-36-53-410Z_pi-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(session), 0o755); err != nil {
		t.Fatal(err)
	}
	photo := base64.StdEncoding.EncodeToString([]byte("jpeg bytes"))
	lines := `{"type":"message","id":"u1","parentId":null,"timestamp":"T1","message":{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","data":"` + photo + `","mimeType":"image/jpeg"}]}}` + "\n"
	if err := os.WriteFile(session, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "hello", "id": "1"})
	resultFor(t, ctx, conn, "1")
	relay.cmux.setAgent(cmux.AgentSession{Kind: "pi", SessionID: "pi-1", Cwd: "/Users/me/app"})
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "attach", "workspace_id": "w1", "surface_id": "t1"})

	chat := readUntil(t, ctx, conn, "chat")
	user := chat["entries"].([]any)[0].(map[string]any)
	if images, _ := user["images"].([]any); len(images) != 1 || images[0] != "u1:1" {
		t.Fatalf("user entry = %v", user)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "2", "image": "u1:1"})
	result := resultFor(t, ctx, conn, "2")
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["format"] != "jpeg" || data["data_b64"] != photo {
		t.Fatalf("chat_image = %v", result)
	}
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "chat_image", "id": "3", "image": "u1:0"})
	if result := resultFor(t, ctx, conn, "3"); errorCode(result) != "not_found" {
		t.Fatalf("a block that isn't an image = %v", result)
	}
}
