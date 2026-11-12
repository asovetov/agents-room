package hub

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// workPaths returns two absolute directories under a temporary root.
//
// They are built with filepath.Join rather than written as "/work/shop" and
// "/work/bot": a POSIX literal is not an absolute path on Windows, where
// filepath.Abs turns it into something under the current drive, and the tests
// then assert about paths the code never saw.
func workPaths(t *testing.T) (shop, bot string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "shop"), filepath.Join(root, "bot")
}

// A broadcast is filed under the session's own repository room, whichever
// other rooms it joined. The room list used to come back in whatever order
// SQLite chose, so a message from a chat in repo:gcs and two shared rooms
// could be attributed to a shared one.
func TestSendFilesTheMessageUnderTheRepositoryRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:gcs", ExtraRooms: []string{"shared:storefront", "storefront"},
		Harness: "test", Label: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(ctx, SendOptions{From: s.ID, To: "*", Subject: "x", Body: "y"}); err != nil {
		t.Fatal(err)
	}

	var room string
	if err := h.db.SQL().QueryRowContext(ctx, `
		SELECT r.name FROM messages m JOIN rooms r ON r.id = m.room_id LIMIT 1`).Scan(&room); err != nil {
		t.Fatal(err)
	}
	if room != "repo:gcs" {
		t.Fatalf("message filed under %q, want the repository room repo:gcs", room)
	}
}

// A relative path names a file in the caller's own checkout. An agent that
// means another repository's file writes the same relative string and leases
// a different file, so the grant has to say so.
func TestClaimWarnsWhenThePathIsOutsideTheCallersDirectory(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	shop, bot := workPaths(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: bot, PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Claim(ctx, s.ID, filepath.Join(shop, "src", "users.go"), 60, ModeExclusive)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !res.OK {
		t.Fatalf("claim denied: %s", res.Reason)
	}
	if res.Warning == "" {
		t.Fatal("no warning for a path outside the caller's directory")
	}
	if !strings.Contains(res.Warning, bot) {
		t.Fatalf("warning does not name the caller's directory: %q", res.Warning)
	}
}

// The ordinary case stays quiet: a path inside your own checkout is what you
// meant.
func TestClaimDoesNotWarnAboutItsOwnFiles(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	_, bot := workPaths(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: bot, PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Claim(ctx, s.ID, filepath.Join(bot, "src", "users.go"), 60, ModeExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning != "" {
		t.Fatalf("unexpected warning for an own file: %q", res.Warning)
	}
}

// A file: selector that matches nobody is the other half of the same trap:
// the pattern is matched against each holder's own checkout, so a relative
// path never reaches a holder in another repository.
func TestSendWarnsWhenAFileSelectorMatchesNobody(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	_, bot := workPaths(t)
	s, err := h.Join(ctx, JoinOptions{
		Room: "repo:bot", Harness: "test", Cwd: bot, PID: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := h.Send(ctx, SendOptions{
		From: s.ID, To: "file:src/users.go", Subject: "need a field", Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 0 {
		t.Fatalf("recipients = %d, want 0", msg.Recipients)
	}
	if msg.Warning == "" {
		t.Fatal("no warning for a file selector that reached nobody")
	}
	if !strings.Contains(msg.Warning, "absolute") {
		t.Fatalf("warning does not say how to fix it: %q", msg.Warning)
	}
}

// A selector that reaches someone is not a mistake, whatever its shape.
func TestSendIsQuietWhenAFileSelectorReachesSomeone(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	shop, _ := workPaths(t)
	holder, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: shop, PID: 4242, Label: "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", ExtraRooms: nil, Harness: "test", Cwd: shop, PID: 4343, Label: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, holder.ID, filepath.Join(shop, "src", "users.go"), 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}
	msg, err := h.Send(ctx, SendOptions{
		From: sender.ID, To: "file:src/users.go", Subject: "x", Body: "y",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d, want 1", msg.Recipients)
	}
	if msg.Warning != "" {
		t.Fatalf("unexpected warning: %q", msg.Warning)
	}
}

// The cross-repository case the tool exists for: a sender in mobile names
// the file as it appears in api's tree, and the selector reaches api's
// holder through the shared room. Before this, the same relative string was
// matched against each holder's checkout and found nobody in another
// repository.
func TestQualifiedFileSelectorReachesAnotherRepository(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	apiDir := t.TempDir()
	mobileDir := t.TempDir()

	api, err := h.Join(ctx, JoinOptions{
		Room: "repo:api", ExtraRooms: []string{"shared:storefront"},
		Cwd: apiDir, Harness: "test", HarnessKey: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := h.Join(ctx, JoinOptions{
		Room: "repo:mobile", ExtraRooms: []string{"shared:storefront"},
		Cwd: mobileDir, Harness: "test", HarnessKey: "mobile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, api.ID, filepath.Join(apiDir, "src", "api", "users.go"), 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	msg, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:api:src/api/users.go", Subject: "need a field", Body: "x",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d, want 1; warning = %q", msg.Recipients, msg.Warning)
	}
	if n, _ := h.Unread(ctx, api.ID); n != 1 {
		t.Fatalf("api holder unread = %d, want 1", n)
	}
	if n, _ := h.Unread(ctx, mobile.ID); n != 0 {
		t.Fatalf("sender received %d messages of its own send", n)
	}
}

// The qualifier names a repository, not a path prefix: a holder in another
// repository must not be reached even when its path ends in the same
// relative string.
func TestQualifiedFileSelectorIgnoresAnotherRepository(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	apiDir := t.TempDir()
	webDir := t.TempDir()

	api, err := h.Join(ctx, JoinOptions{
		Room: "repo:api", ExtraRooms: []string{"shared:storefront"},
		Cwd: apiDir, Harness: "test", HarnessKey: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := h.Join(ctx, JoinOptions{
		Room: "repo:web", ExtraRooms: []string{"shared:storefront"},
		Cwd: webDir, Harness: "test", HarnessKey: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := h.Join(ctx, JoinOptions{
		Room: "repo:mobile", ExtraRooms: []string{"shared:storefront"},
		Cwd: t.TempDir(), Harness: "test", HarnessKey: "mobile",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both checkouts hold src/api/users.go; the selector names api's.
	if _, err := h.Claim(ctx, api.ID, filepath.Join(apiDir, "src", "api", "users.go"), 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, web.ID, filepath.Join(webDir, "src", "api", "users.go"), 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	msg, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:web:src/api/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d, want only the web holder", msg.Recipients)
	}
	if n, _ := h.Unread(ctx, web.ID); n != 1 {
		t.Fatalf("web unread = %d, want 1", n)
	}
	if n, _ := h.Unread(ctx, api.ID); n != 0 {
		t.Fatalf("api received %d messages for a selector naming web", n)
	}
}

// A plain relative selector is matched against every holder's checkout, so it
// reaches every repository whose tree has a file at that relative path -- not
// only the sender's. This is the behaviour the qualified form exists to
// narrow, and it is pinned here so a later "fix" that silently drops the
// cross-repository match is caught.
func TestPlainRelativeSelectorReachesEveryMatchingRepository(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	apiDir := t.TempDir()
	webDir := t.TempDir()

	api, err := h.Join(ctx, JoinOptions{
		Room: "repo:api", ExtraRooms: []string{"shared:storefront"},
		Cwd: apiDir, Harness: "test", HarnessKey: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := h.Join(ctx, JoinOptions{
		Room: "repo:web", ExtraRooms: []string{"shared:storefront"},
		Cwd: webDir, Harness: "test", HarnessKey: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := h.Join(ctx, JoinOptions{
		Room: "repo:mobile", ExtraRooms: []string{"shared:storefront"},
		Cwd: t.TempDir(), Harness: "test", HarnessKey: "mobile",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, holder := range []struct {
		id  string
		dir string
	}{{api.ID, apiDir}, {web.ID, webDir}} {
		if _, err := h.Claim(ctx, holder.id, filepath.Join(holder.dir, "src", "api", "users.go"), 60, ModeExclusive); err != nil {
			t.Fatal(err)
		}
	}

	msg, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:src/api/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 2 {
		t.Fatalf("recipients = %d, want both repositories that have that relative path", msg.Recipients)
	}

	// And the qualified form is what narrows it to one.
	q, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:api:src/api/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.Recipients != 1 {
		t.Fatalf("qualified recipients = %d, want only api", q.Recipients)
	}
}

// A qualified selector reaches nobody when the named repository's holder
// shares no room with the sender. The qualifier says where the file is; it
// does not make an unreachable agent reachable.
func TestQualifiedFileSelectorRequiresASharedRoom(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	apiDir := t.TempDir()
	api, err := h.Join(ctx, JoinOptions{
		Room: "repo:api", Cwd: apiDir, Harness: "test", HarnessKey: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := h.Join(ctx, JoinOptions{
		Room: "repo:mobile", Cwd: t.TempDir(), Harness: "test", HarnessKey: "mobile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, api.ID, filepath.Join(apiDir, "src", "users.go"), 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	msg, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:api:src/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 0 {
		t.Fatalf("recipients = %d, want 0 across repositories with no shared room", msg.Recipients)
	}
	if msg.Warning == "" {
		t.Fatal("no warning when a qualified selector reached nobody")
	}
	if !strings.Contains(msg.Warning, "repo:api") {
		t.Fatalf("warning does not name the repository that was searched: %q", msg.Warning)
	}
}

// An absolute path keeps working, qualified or not: the qualifier is an
// addition, not a replacement.
func TestAbsoluteFileSelectorStillMatches(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	apiDir := t.TempDir()
	api, err := h.Join(ctx, JoinOptions{
		Room: "repo:api", ExtraRooms: []string{"shared:storefront"},
		Cwd: apiDir, Harness: "test", HarnessKey: "api",
	})
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := h.Join(ctx, JoinOptions{
		Room: "repo:mobile", ExtraRooms: []string{"shared:storefront"},
		Cwd: t.TempDir(), Harness: "test", HarnessKey: "mobile",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(apiDir, "src", "api", "users.go")
	if _, err := h.Claim(ctx, api.ID, file, 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	msg, err := h.Send(ctx, SendOptions{
		From: mobile.ID, To: "file:" + file, Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d for an absolute path, want 1; warning = %q",
			msg.Recipients, msg.Warning)
	}
	if n, _ := h.Unread(ctx, api.ID); n != 1 {
		t.Fatalf("api holder unread = %d, want 1", n)
	}
}

// A colon in a path is not a repository qualifier unless it names a room
// that exists. The Windows drive letter is the shape that must not be
// mistaken for one.
func TestDriveLetterIsNotARepositoryQualifier(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	shop, _ := workPaths(t)
	holder, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: shop, PID: 4242, Label: "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: shop, PID: 4343, Label: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(shop, "src", "users.go")
	if _, err := h.Claim(ctx, holder.ID, file, 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	// No room repo:C exists, so the first segment is part of the path and
	// the selector is matched as a plain relative one. It reaches nobody
	// here -- the pattern means C:/src/users.go under the sender's checkout
	// -- and the warning must not claim a repository named C.
	msg, err := h.Send(ctx, SendOptions{
		From: sender.ID, To: "file:C:/src/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 0 {
		t.Fatalf("a drive-letter pattern reached %d, want 0", msg.Recipients)
	}
	if msg.Warning == "" {
		t.Fatal("a pattern with a drive letter that matched nobody got no warning")
	}
	if !strings.Contains(msg.Warning, "file:<repo>:<path>") {
		t.Fatalf("warning does not name the qualified form: %q", msg.Warning)
	}

	// The ordinary relative selector still reaches the holder.
	plain, err := h.Send(ctx, SendOptions{
		From: sender.ID, To: "file:src/users.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Recipients != 1 {
		t.Fatalf("plain relative selector reached %d, want 1", plain.Recipients)
	}
}

// The qualified form is a repository name only when that repository exists.
// A first segment that names no room is a path with a colon in it, and it
// must match exactly as the plain selector would.
func TestUnknownFirstSegmentIsAPath(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)

	shop, _ := workPaths(t)
	holder, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: shop, PID: 4242, Label: "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := h.Join(ctx, JoinOptions{
		Room: "repo:shop", Harness: "test", Cwd: shop, PID: 4343, Label: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The file's relative path itself contains a colon, which is legal on
	// POSIX and is the shape a careless split would mangle.
	file := filepath.Join(shop, "odd:name.go")
	if _, err := h.Claim(ctx, holder.ID, file, 60, ModeExclusive); err != nil {
		t.Fatal(err)
	}

	msg, err := h.Send(ctx, SendOptions{
		From: sender.ID, To: "file:odd:name.go", Subject: "s", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipients != 1 {
		t.Fatalf("recipients = %d, want 1: an unknown first segment must stay a path; warning = %q",
			msg.Recipients, msg.Warning)
	}
	if n, _ := h.Unread(ctx, holder.ID); n != 1 {
		t.Fatalf("holder unread = %d, want 1", n)
	}
}
