package imapserver_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

var literalSuffixRe = regexp.MustCompile(`\{(\d+)\}\r\n$`)

// rawConn is a minimal raw IMAP protocol client used to assert the exact wire
// format produced by the server in UIDONLY mode.
type rawConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	tagN int
}

func dialRawConn(t *testing.T, addr string) *rawConn {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	return &rawConn{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func (c *rawConn) close() {
	c.conn.Close()
}

// readFrame reads one response frame, consuming the exact number of literal
// bytes announced by a "{n}" marker so that literal content (which may contain
// CRLFs) is never mistaken for response lines.
func (c *rawConn) readFrame() string {
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read response: %v", err)
	}
	var sb strings.Builder
	sb.Write(line)
	if m := literalSuffixRe.FindSubmatch(line); m != nil {
		var n int
		fmt.Sscanf(string(m[1]), "%d", &n)
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.br, buf); err != nil {
			c.t.Fatalf("read %d literal bytes: %v", n, err)
		}
		sb.Write(buf)
		tail, err := c.br.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("read literal tail: %v", err)
		}
		sb.Write(tail)
	}
	return sb.String()
}

func (c *rawConn) nextTag() string {
	c.tagN++
	return fmt.Sprintf("t%d", c.tagN)
}

func (c *rawConn) writeRaw(s string) {
	if _, err := io.WriteString(c.conn, s); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// cmd sends a command and returns all frames up to and including the tagged
// completion response.
func (c *rawConn) cmd(format string, args ...any) []string {
	tag := c.nextTag()
	c.writeRaw(fmt.Sprintf("%s %s\r\n", tag, fmt.Sprintf(format, args...)))
	return c.readUntilTag(tag)
}

func (c *rawConn) readUntilTag(tag string) []string {
	var frames []string
	for {
		frame := c.readFrame()
		frames = append(frames, frame)
		if strings.HasPrefix(frame, tag+" ") {
			return frames
		}
	}
}

func (c *rawConn) greeting() string {
	return c.readFrame()
}

func (c *rawConn) login(username, password string) []string {
	return c.cmd("LOGIN %s %s", username, password)
}

func (c *rawConn) enable(caps ...string) []string {
	return c.cmd("ENABLE %s", strings.Join(caps, " "))
}

func (c *rawConn) selectMailbox(name string) []string {
	return c.cmd("SELECT %s", name)
}

func (c *rawConn) append(mailbox, body string) []string {
	tag := c.nextTag()
	c.writeRaw(fmt.Sprintf("%s APPEND %s () {%d}\r\n", tag, mailbox, len(body)))
	cont := c.readFrame()
	if !strings.HasPrefix(cont, "+ ") {
		c.t.Fatalf("expected continuation request, got %q", cont)
	}
	c.writeRaw(body + "\r\n")
	return c.readUntilTag(tag)
}

func (c *rawConn) idleStart() string {
	tag := c.nextTag()
	c.writeRaw(tag + " IDLE\r\n")
	cont := c.readFrame()
	if !strings.HasPrefix(cont, "+ ") {
		c.t.Fatalf("expected IDLE continuation, got %q", cont)
	}
	return tag
}

func (c *rawConn) idleWait(maxWait time.Duration) []string {
	c.conn.SetReadDeadline(time.Now().Add(maxWait))
	var frames []string
	for {
		line, err := c.br.ReadBytes('\n')
		if err != nil {
			break
		}
		frames = append(frames, string(line))
		c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	}
	c.conn.SetReadDeadline(time.Time{})
	return frames
}

func (c *rawConn) idleDone(tag string) []string {
	c.writeRaw("DONE\r\n")
	return c.readUntilTag(tag)
}

func joinFrames(frames []string) string {
	return strings.Join(frames, "")
}

func expectContains(t *testing.T, frames []string, wants ...string) {
	t.Helper()
	got := joinFrames(frames)
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("response does not contain %q\nfull response:\n%s", want, got)
		}
	}
}

func expectNotContains(t *testing.T, frames []string, notWants ...string) {
	t.Helper()
	got := joinFrames(frames)
	for _, want := range notWants {
		if strings.Contains(got, want) {
			t.Errorf("response unexpectedly contains %q\nfull response:\n%s", want, got)
		}
	}
}

var (
	untaggedExpungeRe = regexp.MustCompile(`(?m)^\* \d+ EXPUNGE\r$`)
	vanishedRe        = regexp.MustCompile(`(?m)^\* VANISHED ([0-9:,]+)\r$`)
)

// expectNoExpungeFrame asserts that no untagged "* N EXPUNGE" response is
// present (the tagged completion text of an EXPUNGE command legitimately
// contains the word "EXPUNGE").
func expectNoExpungeFrame(t *testing.T, frames []string) {
	t.Helper()
	got := joinFrames(frames)
	if loc := untaggedExpungeRe.FindString(got); loc != "" {
		t.Errorf("unexpected untagged EXPUNGE frame %q\nfull response:\n%s", loc, got)
	}
}

// vanishedUIDCounts parses all VANISHED responses and returns how many times
// each UID was announced. The server is allowed to split a batch across
// several VANISHED responses, but each UID must appear exactly once.
func vanishedUIDCounts(t *testing.T, frames []string) map[uint32]int {
	t.Helper()
	counts := make(map[uint32]int)
	for _, m := range vanishedRe.FindAllStringSubmatch(joinFrames(frames), -1) {
		for _, part := range strings.Split(m[1], ",") {
			lo, hi, _ := strings.Cut(part, ":")
			start, err := strconv.ParseUint(lo, 10, 32)
			if err != nil {
				t.Fatalf("malformed VANISHED response %q: %v", m[0], err)
			}
			stop := start
			if hi != "" {
				stop, err = strconv.ParseUint(hi, 10, 32)
				if err != nil {
					t.Fatalf("malformed VANISHED response %q: %v", m[0], err)
				}
			}
			for u := start; u <= stop; u++ {
				counts[uint32(u)]++
			}
		}
	}
	return counts
}

// expectVanishedExactly asserts that the VANISHED responses announce exactly
// the provided UIDs, each exactly once, regardless of how the UIDs are split
// across responses.
func expectVanishedExactly(t *testing.T, frames []string, want ...uint32) {
	t.Helper()
	counts := vanishedUIDCounts(t, frames)
	if len(counts) != len(want) {
		t.Errorf("VANISHED: got %d distinct UIDs, want %d\nfull response:\n%s", len(counts), len(want), joinFrames(frames))
	}
	for _, u := range want {
		switch counts[u] {
		case 0:
			t.Errorf("VANISHED: missing UID %d\nfull response:\n%s", u, joinFrames(frames))
		case 1:
			// ok
		default:
			t.Errorf("VANISHED: UID %d announced %d times\nfull response:\n%s", u, counts[u], joinFrames(frames))
		}
	}
}

// unauthenticateSession wraps a memory backend session to add UNAUTHENTICATE
// support (RFC 8437).
type unauthenticateSession struct {
	imapserver.SessionIMAP4rev2
}

func (*unauthenticateSession) Unauthenticate() error { return nil }

type uidOnlyTestServer struct {
	mem *imapmemserver.Server
	ln  net.Listener
	srv *imapserver.Server
}

var uidOnlyDefaultCaps = imap.CapSet{
	imap.CapIMAP4rev1: {},
	imap.CapIMAP4rev2: {},
	imap.CapMove:      {},
	imap.CapUIDPlus:   {},
	imap.CapUIDOnly:   {},
}

func startUIDOnlyTestServer(t *testing.T, caps imap.CapSet, unauthenticate bool) (*uidOnlyTestServer, string) {
	t.Helper()

	mem := imapmemserver.New()
	user := imapmemserver.NewUser("test-user", "test-password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("Dest", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			var sess imapserver.Session = mem.NewSession()
			if unauthenticate {
				sess = &unauthenticateSession{SessionIMAP4rev2: sess.(imapserver.SessionIMAP4rev2)}
			}
			return sess, nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	ts := &uidOnlyTestServer{mem: mem, ln: ln, srv: srv}
	t.Cleanup(func() {
		srv.Close()
	})
	return ts, ln.Addr().String()
}

func newEnabledClient(t *testing.T, addr string) *rawConn {
	t.Helper()
	c := dialRawConn(t, addr)
	c.greeting()
	frames := c.login("test-user", "test-password")
	expectContains(t, frames, " UIDONLY")
	if frames := c.enable("UIDONLY"); !strings.Contains(joinFrames(frames), "* ENABLED UIDONLY\r\n") {
		t.Fatalf("ENABLE UIDONLY not acknowledged: %q", joinFrames(frames))
	}
	c.selectMailbox("INBOX")
	return c
}

// appendMessages appends n messages with deterministic bodies. The UID of
// the first appended message is startUID.
func appendMessages(t *testing.T, c *rawConn, n int, startUID int) {
	t.Helper()
	for i := 0; i < n; i++ {
		uid := startUID + i
		body := fmt.Sprintf("Subject: message %d\r\n\r\nbody %d\r\n", uid, uid)
		frames := c.append("INBOX", body)
		// EXISTS reports a message count, not a UID; assert the UID
		// assignment via APPENDUID here.
		expectContains(t, frames, fmt.Sprintf("[APPENDUID 1 %d]", uid))
	}
}

func TestUIDOnlyCapabilityAndEnable(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)

	// UIDONLY is not advertised before authentication, but is advertised to
	// authenticated clients.
	c1 := dialRawConn(t, addr)
	defer c1.close()
	greeting := c1.greeting()
	if strings.Contains(greeting, "UIDONLY") {
		t.Errorf("UIDONLY must not be advertised pre-authentication: %q", greeting)
	}
	login := c1.login("test-user", "test-password")
	expectContains(t, login, " UIDONLY")
	if frames := c1.enable("UIDONLY"); !strings.Contains(joinFrames(frames), "* ENABLED UIDONLY\r\n") {
		t.Fatalf("missing * ENABLED UIDONLY: %q", joinFrames(frames))
	}

	// An independent connection which does not ENABLE UIDONLY is unaffected.
	c2 := dialRawConn(t, addr)
	defer c2.close()
	c2.greeting()
	c2.login("test-user", "test-password")
	c2.selectMailbox("INBOX")
	appendMessages(t, c2, 1, 1)
	frames := c2.cmd("FETCH 1 (UID FLAGS)")
	expectContains(t, frames, "* 1 FETCH (UID 1 FLAGS ())", "OK")
	expectNotContains(t, frames, "UIDFETCH")
}

func TestUIDOnlyNotConfigured(t *testing.T) {
	// UIDONLY not present in Caps: it must not be advertised, ENABLE
	// UIDONLY must not enable anything, and sequence numbers keep working.
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
	_, addr := startUIDOnlyTestServer(t, caps, false)

	c := dialRawConn(t, addr)
	defer c.close()
	c.greeting()
	login := c.login("test-user", "test-password")
	if strings.Contains(joinFrames(login), "UIDONLY") {
		t.Fatalf("UIDONLY advertised while not configured: %q", joinFrames(login))
	}
	enabled := c.enable("UIDONLY")
	if strings.Contains(joinFrames(enabled), "UIDONLY") {
		t.Fatalf("UIDONLY enabled while not advertised: %q", joinFrames(enabled))
	}
	c.selectMailbox("INBOX")
	appendMessages(t, c, 1, 1)
	frames := c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "* 1 FETCH (UID 1)")
}

func TestUIDOnlyStatePersistsAcrossMailboxLifecycle(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 2, 1)

	// Switching mailboxes keeps UIDONLY enabled.
	c.selectMailbox("Dest")
	frames := c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "BAD [UIDREQUIRED]")

	c.selectMailbox("INBOX")
	frames = c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "BAD [UIDREQUIRED]")

	// UNSELECT leaves UIDONLY enabled.
	frames = c.cmd("UNSELECT")
	expectContains(t, frames, "OK")
	frames = c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "BAD [UIDREQUIRED]")

	// CLOSE keeps UIDONLY enabled.
	c.selectMailbox("INBOX")
	c.cmd("UID STORE 1 +FLAGS (\\Deleted)")
	frames = c.cmd("CLOSE")
	expectContains(t, frames, "OK")
	frames = c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "BAD [UIDREQUIRED]")
}

func TestUIDOnlyStateResetByUnauthenticate(t *testing.T) {
	caps := imap.CapSet{
		imap.CapIMAP4rev1:      {},
		imap.CapIMAP4rev2:      {},
		imap.CapMove:           {},
		imap.CapUIDPlus:        {},
		imap.CapUIDOnly:        {},
		imap.CapUnauthenticate: {},
	}
	_, addr := startUIDOnlyTestServer(t, caps, true)

	c := dialRawConn(t, addr)
	defer c.close()
	c.greeting()
	c.login("test-user", "test-password")
	c.enable("UIDONLY")
	c.selectMailbox("INBOX")
	expectContains(t, c.cmd("FETCH 1 (UID)"), "BAD [UIDREQUIRED]")

	// UNAUTHENTICATE clears all enabled extensions.
	expectContains(t, c.cmd("UNAUTHENTICATE"), "OK")

	// Re-authenticating leaves the connection in a clean state: sequence
	// numbers work again, and UIDONLY must be re-enabled explicitly.
	c.login("test-user", "test-password")
	c.selectMailbox("INBOX")
	frames := c.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "FETCH")
	expectNotContains(t, frames, "UIDREQUIRED", "UIDFETCH")

	frames = c.enable("UIDONLY")
	expectContains(t, frames, "* ENABLED UIDONLY\r\n")
	expectContains(t, c.cmd("FETCH 1 (UID)"), "BAD [UIDREQUIRED]")
}

func TestUIDOnlyRejectsSequenceCommands(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 4, 1)

	for _, cmd := range []string{
		"FETCH 1 (UID)",
		"STORE 1 +FLAGS (\\Seen)",
		"SEARCH ALL",
		"COPY 1 Dest",
		"MOVE 1 Dest",
	} {
		frames := c.cmd("%s", cmd)
		expectContains(t, frames, "BAD [UIDREQUIRED]")
	}

	// Sequence number criteria are rejected at every nesting level of a
	// UID SEARCH command.
	for _, key := range []string{
		"1",
		"1:*",
		"2,4",
		"NOT 2",
		"OR 1 2",
		"(3)",
		"(OR (NOT 1) (2))",
		"NOT (OR 1 ALL)",
		"OR ALL (NOT 4)",
	} {
		frames := c.cmd("UID SEARCH %s", key)
		expectContains(t, frames, "BAD [UIDREQUIRED]")
	}

	// ALL, UID criteria (including UID wildcard ranges) and nested
	// combinations without sequence numbers remain valid.
	for _, key := range []string{
		"ALL",
		"(ALL)",
		"UID 1:*",
		"UID 2",
		"NOT UID 2",
		"OR UID 1 UID 3",
		"ANSWERED",
		"NOT DELETED",
	} {
		frames := c.cmd("UID SEARCH %s", key)
		expectContains(t, frames, "OK")
		expectNotContains(t, frames, "UIDREQUIRED")
	}

	// A rejected command does not affect the next valid command.
	frames := c.cmd("UID FETCH 2 (UID FLAGS)")
	expectContains(t, frames, "* 2 UIDFETCH (UID 2 FLAGS ())", "OK")

	// UID COPY is allowed and keeps the COPYUID response code.
	frames = c.cmd("UID COPY 1 Dest")
	expectContains(t, frames, "OK [COPYUID", "COPY completed")
	expectNoExpungeFrame(t, frames)
	expectNotContains(t, frames, "UIDFETCH", "VANISHED")
}

func TestUIDOnlyFetchResponses(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()

	body := "Subject: hi\r\n\r\nline1\r\nline2\r\n"
	c.append("INBOX", body)

	// The UID data item is not included unless explicitly requested; the
	// leading number is the real UID.
	frames := c.cmd("UID FETCH 1 (FLAGS)")
	expectContains(t, frames, "* 1 UIDFETCH (FLAGS ())\r\n", "OK")
	expectNotContains(t, frames, "UID 1")

	frames = c.cmd("UID FETCH 1 (RFC822.SIZE FLAGS)")
	expectContains(t, frames, fmt.Sprintf("* 1 UIDFETCH (FLAGS () RFC822.SIZE %d)\r\n", len(body)))

	// Explicitly requested UID and the body literal are preserved, with the
	// exact literal bytes and boundary.
	frames = c.cmd("UID FETCH 1 (UID FLAGS BODY[])")
	expectContains(t, frames,
		fmt.Sprintf("* 1 UIDFETCH (UID 1 FLAGS (\\seen) BODY[] {%d}\r\n%s)\r\n", len(body), body),
		"OK",
	)
}

func TestUIDOnlyUIDNotEqualToSequence(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 3, 1)

	// Expunge UID 1: sequence numbers shift, but UIDs stay stable.
	c.cmd("UID STORE 1 +FLAGS (\\Deleted)")
	frames := c.cmd("EXPUNGE")
	expectContains(t, frames, "* VANISHED 1\r\n", "OK")
	expectNoExpungeFrame(t, frames)

	frames = c.cmd("UID FETCH 1:* (UID)")
	expectContains(t, frames,
		"* 2 UIDFETCH (UID 2)\r\n",
		"* 3 UIDFETCH (UID 3)\r\n",
	)
	expectNotContains(t, frames, "UID 1")

	// UID 2 is now sequence number 1 for a classic client.
	c2 := dialRawConn(t, addr)
	defer c2.close()
	c2.greeting()
	c2.login("test-user", "test-password")
	c2.selectMailbox("INBOX")
	frames = c2.cmd("FETCH 1 (UID)")
	expectContains(t, frames, "* 1 FETCH (UID 2)")
}

func TestUIDOnlyExpungeCoalescing(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 4, 1)

	c.cmd("UID STORE 1,2,4 +FLAGS (\\Deleted)")
	frames := c.cmd("EXPUNGE")
	expectContains(t, frames, "* VANISHED 1:2,4\r\n", "OK")
	expectNoExpungeFrame(t, frames)
	expectNotContains(t, frames, "UIDFETCH")

	// UID EXPUNGE is filtered by UID and announced with VANISHED.
	appendMessages(t, c, 3, 5) // now UIDs 3,5,6,7
	c.cmd("UID STORE 5,6 +FLAGS (\\Deleted)")
	frames = c.cmd("UID EXPUNGE 5")
	expectContains(t, frames, "* VANISHED 5\r\n", "OK")
	expectNoExpungeFrame(t, frames)
	frames = c.cmd("UID EXPUNGE 6:7")
	// 7 is not marked as deleted, only 6 is expunged.
	expectContains(t, frames, "* VANISHED 6\r\n", "OK")
	expectNoExpungeFrame(t, frames)

	frames = c.cmd("UID FETCH 1:* (UID)")
	expectContains(t, frames, "* 3 UIDFETCH (UID 3)\r\n", "* 7 UIDFETCH (UID 7)\r\n")
	expectNotContains(t, frames, "UID 5", "UID 6")
}

func TestUIDOnlyMove(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 3, 1)

	frames := c.cmd("UID MOVE 1:3 Dest")
	expectContains(t, frames,
		"* OK [COPYUID",
		" 1:3 ",
		"* VANISHED 1:3\r\n",
		"OK",
	)
	expectNotContains(t, frames, "EXPUNGE", "* 0 ")

	c.selectMailbox("Dest")
	frames = c.cmd("UID FETCH 1:* (UID FLAGS)")
	expectContains(t, frames,
		"* 1 UIDFETCH (UID 1 FLAGS ())\r\n",
		"* 2 UIDFETCH (UID 2 FLAGS ())\r\n",
		"* 3 UIDFETCH (UID 3 FLAGS ())\r\n",
	)
}

func TestUIDOnlyAsyncUpdates(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c1 := newEnabledClient(t, addr)
	defer c1.close()
	appendMessages(t, c1, 4, 1)

	// Classic connection modifying the mailbox concurrently.
	c2 := dialRawConn(t, addr)
	defer c2.close()
	c2.greeting()
	c2.login("test-user", "test-password")
	c2.selectMailbox("INBOX")

	// Flag change made by another connection is pushed as UIDFETCH with the
	// real leading UID.
	c2.cmd("UID STORE 3 +FLAGS (\\Flagged)")
	frames := c1.cmd("NOOP")
	expectContains(t, frames, "* 3 UIDFETCH (FLAGS (\\flagged))\r\n")
	expectNotContains(t, frames, "UID 3")

	// Deletions made by another connection come back as VANISHED with the
	// real UIDs even after sequence numbers shift.
	c2.cmd("UID STORE 1,2 +FLAGS (\\Deleted)")
	c2.cmd("EXPUNGE")
	frames = c1.cmd("NOOP")
	expectContains(t, frames, "* VANISHED 1:2\r\n")
	expectNoExpungeFrame(t, frames)

	// IDLE pushes the same information asynchronously.
	tag := c1.idleStart()
	c2.cmd("UID STORE 4 +FLAGS (\\Deleted)")
	c2.cmd("EXPUNGE")
	frames = c1.idleWait(2 * time.Second)
	expectContains(t, frames, "* VANISHED 4\r\n")
	expectNoExpungeFrame(t, frames)
	expectNotContains(t, frames, "* 0 ")
	frames = c1.idleDone(tag)
	expectContains(t, frames, "OK")

	// Remaining message keeps its stable UID.
	frames = c1.cmd("UID FETCH 1:* (UID FLAGS)")
	expectContains(t, frames, "* 3 UIDFETCH (UID 3 FLAGS (\\flagged))\r\n")
}

func TestUIDOnlyAsyncMultipleDeletes(t *testing.T) {
	// Multiple consecutive deletions performed by another connection must
	// not be misidentified, duplicated or lost when delivered late (Poll) or
	// via IDLE.
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c1 := newEnabledClient(t, addr)
	defer c1.close()
	appendMessages(t, c1, 6, 1)

	c2 := dialRawConn(t, addr)
	defer c2.close()
	c2.greeting()
	c2.login("test-user", "test-password")
	c2.selectMailbox("INBOX")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c2.cmd("UID STORE 1,2,3,4,5,6 +FLAGS (\\Deleted)")
		c2.cmd("EXPUNGE")
	}()
	wg.Wait()

	// Delayed poll delivers every deleted UID exactly once, possibly split
	// across several VANISHED responses.
	frames := c1.cmd("NOOP")
	expectVanishedExactly(t, frames, 1, 2, 3, 4, 5, 6)
	expectNoExpungeFrame(t, frames)

	// Same through IDLE: append more messages, delete them while c1 is
	// idling.
	appendMessages(t, c1, 3, 7)
	tag := c1.idleStart()
	c2.cmd("UID STORE 7,8,9 +FLAGS (\\Deleted)")
	c2.cmd("EXPUNGE")
	frames = c1.idleWait(2 * time.Second)
	expectVanishedExactly(t, frames, 7, 8, 9)
	expectNoExpungeFrame(t, frames)
	expectNotContains(t, frames, "* 0 ")
	frames = c1.idleDone(tag)
	expectContains(t, frames, "OK")
}

func TestUIDOnlyExistsStillCount(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)
	c := newEnabledClient(t, addr)
	defer c.close()
	appendMessages(t, c, 2, 1)

	frames := c.selectMailbox("INBOX")
	expectContains(t, frames, "* 2 EXISTS\r\n")
	expectNotContains(t, frames, "VANISHED", "UIDFETCH")

	c.cmd("UID STORE 1 +FLAGS (\\Deleted)")
	frames = c.cmd("EXPUNGE")
	expectContains(t, frames, "* VANISHED 1\r\n")
	// EXISTS is not emitted on EXPUNGE by the memory backend, but when a new
	// message arrives it still reports the new count.
	frames = c.append("INBOX", "Subject: new\r\n\r\nx")
	expectContains(t, frames, "* 2 EXISTS\r\n")
}

func TestUIDOnlyClassicSessionCompatibility(t *testing.T) {
	_, addr := startUIDOnlyTestServer(t, uidOnlyDefaultCaps, false)

	// A connection on a UIDONLY-capable server which never enables the mode
	// keeps the classic FETCH/EXPUNGE/sequence-number behavior.
	c := dialRawConn(t, addr)
	defer c.close()
	c.greeting()
	c.login("test-user", "test-password")
	c.selectMailbox("INBOX")
	appendMessages(t, c, 2, 1)

	frames := c.cmd("STORE 1 +FLAGS (\\Deleted)")
	expectContains(t, frames, "* 1 FETCH (UID 1 FLAGS (\\deleted))\r\n")
	expectNotContains(t, frames, "UIDFETCH", "VANISHED")

	frames = c.cmd("COPY 2 Dest")
	expectContains(t, frames, "OK [COPYUID", "COPY completed")

	frames = c.cmd("SEARCH ALL")
	expectContains(t, frames, "* SEARCH 1 2\r\n")

	frames = c.cmd("EXPUNGE")
	expectContains(t, frames, "* 1 EXPUNGE\r\n", "OK")
	expectNotContains(t, frames, "VANISHED", "UIDFETCH")

	// UID commands still work and return UIDs explicitly.
	frames = c.cmd("UID FETCH 2 (UID FLAGS)")
	expectContains(t, frames, "* 1 FETCH (UID 2 FLAGS ())\r\n")
}

func TestUIDOnlyEnabledUnderIMAP4rev1(t *testing.T) {
	// UIDONLY must also work with an IMAP4rev1-only server configuration.
	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapMove:      {},
		imap.CapUIDPlus:   {},
		imap.CapUIDOnly:   {},
	}
	_, addr := startUIDOnlyTestServer(t, caps, false)

	c := dialRawConn(t, addr)
	defer c.close()
	c.greeting()
	login := c.login("test-user", "test-password")
	expectContains(t, login, " ENABLE", " UIDONLY")
	frames := c.enable("UIDONLY")
	expectContains(t, frames, "* ENABLED UIDONLY\r\n")
	c.selectMailbox("INBOX")
	appendMessages(t, c, 1, 1)
	frames = c.cmd("UID FETCH 1 (UID FLAGS)")
	expectContains(t, frames, "* 1 UIDFETCH (UID 1 FLAGS ())\r\n")
}
