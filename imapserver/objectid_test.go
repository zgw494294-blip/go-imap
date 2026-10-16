package imapserver_test

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	objectidTestUsername = "user"
	objectidTestPassword = "pass"
)

var (
	objectIDRe  = regexp.MustCompile(`MAILBOXID \(([A-Za-z0-9_-]+)\)`)
	fetchUIDRe  = regexp.MustCompile(`UID (\d+)`)
	emailIDRe   = regexp.MustCompile(`EMAILID \(([A-Za-z0-9_-]+)\)`)
	threadIDRe  = regexp.MustCompile(`THREADID (?:\(([A-Za-z0-9_-]+)\)|(NIL))`)
	copyUIDRe   = regexp.MustCompile(`\[COPYUID `)
	capObjectRe = regexp.MustCompile(`(?:^| )OBJECTID(?: |$)`)
)

func startObjectIDTestServer(t *testing.T, withObjectID bool) (*imapserver.Server, string) {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(objectidTestUsername, objectidTestPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("User.Create(INBOX) = %v", err)
	}
	memServer.AddUser(user)

	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
	}
	if withObjectID {
		caps[imap.CapObjectID] = struct{}{}
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Close()
	})
	return server, ln.Addr().String()
}

type rawIMAP struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func dialRawIMAP(t *testing.T, addr string) *rawIMAP {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	c := &rawIMAP{t: t, conn: conn, br: bufio.NewReader(conn)}
	greeting := c.readLine()
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("bad greeting: %q", greeting)
	}
	c.commandOK("LOGIN %s %s", objectidTestUsername, objectidTestPassword)
	return c
}

func (c *rawIMAP) close() {
	c.conn.Close()
}

func (c *rawIMAP) readLine() string {
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read response: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func (c *rawIMAP) nextTag() string {
	c.tag++
	return fmt.Sprintf("t%d", c.tag)
}

// command sends a command and returns the untagged responses and the tagged
// status response.
func (c *rawIMAP) command(format string, args ...interface{}) (untagged []string, tagged string) {
	tag := c.nextTag()
	cmd := fmt.Sprintf(format, args...)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, cmd); err != nil {
		c.t.Fatalf("write command %q: %v", cmd, err)
	}
	return c.finish(tag)
}

func (c *rawIMAP) finish(tag string) (untagged []string, tagged string) {
	for {
		line := c.readLine()
		if strings.HasPrefix(line, tag+" ") {
			return untagged, line
		}
		untagged = append(untagged, line)
	}
}

func (c *rawIMAP) commandOK(format string, args ...interface{}) []string {
	untagged, tagged := c.command(format, args...)
	if !strings.Contains(tagged, " OK ") {
		c.t.Fatalf("command %q failed: %s", fmt.Sprintf(format, args...), tagged)
	}
	return untagged
}

func (c *rawIMAP) commandBad(format string, args ...interface{}) {
	_, tagged := c.command(format, args...)
	if !strings.Contains(tagged, " BAD ") {
		c.t.Fatalf("command %q: want BAD, got %q", fmt.Sprintf(format, args...), tagged)
	}
}

func (c *rawIMAP) commandNo(format string, args ...interface{}) {
	_, tagged := c.command(format, args...)
	if !strings.Contains(tagged, " NO ") {
		c.t.Fatalf("command %q: want NO, got %q", fmt.Sprintf(format, args...), tagged)
	}
}

// append sends an APPEND command with a literal payload.
func (c *rawIMAP) append(mailbox string, msg []byte) {
	tag := c.nextTag()
	if _, err := fmt.Fprintf(c.conn, "%s APPEND %s () \"01-Jan-2026 00:00:00 +0000\" {%d}\r\n", tag, mailbox, len(msg)); err != nil {
		c.t.Fatalf("write APPEND: %v", err)
	}
	cont := c.readLine()
	if !strings.HasPrefix(cont, "+") {
		c.t.Fatalf("want continuation request, got %q", cont)
	}
	if _, err := c.conn.Write(msg); err != nil {
		c.t.Fatalf("write literal: %v", err)
	}
	if _, err := c.conn.Write([]byte("\r\n")); err != nil {
		c.t.Fatalf("write literal CRLF: %v", err)
	}
	_, tagged := c.finish(tag)
	if !strings.Contains(tagged, " OK ") {
		c.t.Fatalf("APPEND failed: %s", tagged)
	}
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

func rawMessage(subject, body string) []byte {
	return []byte(fmt.Sprintf("Subject: %s\r\n\r\n%s\r\n", subject, body))
}

// TestServerObjectID exercises RFC 8474 OBJECTID on top of the in-memory
// backend through the wire protocol.
func TestServerObjectID(t *testing.T) {
	_, addr := startObjectIDTestServer(t, true)
	c := dialRawIMAP(t, addr)
	defer c.close()

	// Capability is negotiated.
	untagged := c.commandOK("CAPABILITY")
	if !capObjectRe.MatchString(strings.Join(untagged, "\n")) {
		t.Fatalf("OBJECTID not advertised in CAPABILITY: %v", untagged)
	}

	// CREATE returns MAILBOXID in the tagged response.
	_, tagged := c.command("CREATE foo")
	idCreate := firstSubmatch(objectIDRe, tagged)
	if idCreate == "" {
		t.Fatalf("CREATE response has no MAILBOXID: %q", tagged)
	}
	if !strings.Contains(tagged, " OK ") {
		t.Fatalf("CREATE failed: %q", tagged)
	}

	// STATUS returns the same identifier.
	untagged = c.commandOK("STATUS foo (MAILBOXID UIDVALIDITY)")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idCreate {
		t.Fatalf("STATUS MAILBOXID = %q, want %q", id, idCreate)
	}

	// SELECT and EXAMINE return the same identifier in an untagged response.
	untagged = c.commandOK("SELECT foo")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idCreate {
		t.Fatalf("SELECT MAILBOXID = %q, want %q", id, idCreate)
	}
	untagged = c.commandOK("EXAMINE foo")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idCreate {
		t.Fatalf("EXAMINE MAILBOXID = %q, want %q", id, idCreate)
	}

	// Distinct mailboxes get distinct identifiers.
	_, tagged = c.command("CREATE bar")
	idBar := firstSubmatch(objectIDRe, tagged)
	if idBar == "" || idBar == idCreate {
		t.Fatalf("CREATE bar MAILBOXID = %q, want a distinct id", idBar)
	}

	// A duplicate CREATE is a tagged NO without MAILBOXID.
	c.commandNo("CREATE foo")

	// RENAME keeps the identifier stable, the old name stops resolving.
	c.commandOK("RENAME foo renamed")
	untagged = c.commandOK("STATUS renamed (MAILBOXID)")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idCreate {
		t.Fatalf("STATUS renamed MAILBOXID = %q, want %q", id, idCreate)
	}
	c.commandNo("STATUS foo (MAILBOXID)")

	// Delete + recreate with the same name produces a fresh identifier.
	c.commandOK("DELETE renamed")
	_, tagged = c.command("CREATE renamed")
	idRenamedNew := firstSubmatch(objectIDRe, tagged)
	if idRenamedNew == "" || idRenamedNew == idCreate {
		t.Fatalf("recreated MAILBOXID = %q, want a fresh id", idRenamedNew)
	}

	c.commandOK("DELETE bar")
	_, tagged = c.command("CREATE bar")
	if id := firstSubmatch(objectIDRe, tagged); id == "" || id == idBar {
		t.Fatalf("recreated bar MAILBOXID = %q, want a fresh id", id)
	}

	// Another session sees the surviving object, not a stale name cache.
	c2 := dialRawIMAP(t, addr)
	defer c2.close()
	untagged = c2.commandOK("STATUS renamed (MAILBOXID)")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idRenamedNew {
		t.Fatalf("second session MAILBOXID = %q, want %q", id, idRenamedNew)
	}

	// A mailbox created in one session and renamed there is immediately
	// resolvable under its new name from another session, with the same id.
	_, tagged = c2.command("CREATE shared")
	idShared := firstSubmatch(objectIDRe, tagged)
	if idShared == "" {
		t.Fatalf("CREATE shared has no MAILBOXID: %q", tagged)
	}
	c2.commandOK("RENAME shared shared2")
	untagged = c.commandOK("STATUS shared2 (MAILBOXID)")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idShared {
		t.Fatalf("renamed shared2 MAILBOXID = %q, want %q", id, idShared)
	}
	c.commandNo("STATUS shared (MAILBOXID)")

	// LIST-STATUS carries MAILBOXID for the live mailboxes.
	untagged = c.commandOK(`LIST "" "*" RETURN (STATUS (MAILBOXID))`)
	var sawRenamedInList bool
	for _, line := range untagged {
		if strings.HasPrefix(line, "* STATUS ") {
			if !strings.Contains(line, "MAILBOXID (") {
				t.Fatalf("LIST-STATUS line missing MAILBOXID: %q", line)
			}
			if strings.Contains(line, "renamed") && strings.Contains(line, idRenamedNew) {
				sawRenamedInList = true
			}
		}
	}
	if !sawRenamedInList {
		t.Fatalf("LIST-STATUS did not report renamed: %v", untagged)
	}

	// The first session previously had "foo" selected. Re-selecting the
	// reused name resolves to the live mailbox.
	untagged = c.commandOK("SELECT renamed")
	if id := firstSubmatch(objectIDRe, strings.Join(untagged, "\n")); id != idRenamedNew {
		t.Fatalf("re-selected MAILBOXID = %q, want %q", id, idRenamedNew)
	}

	// Append messages and check EMAILID/THREADID.
	msgA := rawMessage("Message A", "hello A")
	msgB := rawMessage("Message B", "hello B")
	c.append("INBOX", msgA)
	c.append("INBOX", msgB)

	untagged = c.commandOK("SELECT INBOX")
	join := strings.Join(untagged, "\n")
	if !strings.Contains(join, "EXISTS") || !strings.Contains(join, "UIDVALIDITY") || !strings.Contains(join, "UIDNEXT") {
		t.Fatalf("SELECT INBOX missing standard responses: %v", untagged)
	}

	untagged = c.commandOK("FETCH 1:* (UID EMAILID THREADID)")
	if len(untagged) != 2 {
		t.Fatalf("FETCH returned %d responses, want 2: %v", len(untagged), untagged)
	}
	eA := firstSubmatch(emailIDRe, untagged[0])
	eB := firstSubmatch(emailIDRe, untagged[1])
	if eA == "" || eB == "" || eA == eB {
		t.Fatalf("bad EMAILIDs: %q, %q", eA, eB)
	}
	for _, line := range untagged {
		if id := firstSubmatch(threadIDRe, line); id != "NIL" {
			t.Fatalf("THREADID = %q, want NIL: %q", id, line)
		}
	}

	// Repeated reads are stable.
	untagged = c.commandOK("FETCH 1 (EMAILID)")
	if id := firstSubmatch(emailIDRe, strings.Join(untagged, "\n")); id != eA {
		t.Fatalf("re-fetch EMAILID = %q, want %q", id, eA)
	}
	untagged = c.commandOK("UID FETCH 1:2 (UID EMAILID THREADID)")
	ids := map[string]bool{}
	for _, line := range untagged {
		ids[firstSubmatch(emailIDRe, line)] = true
		if id := firstSubmatch(threadIDRe, line); id != "NIL" {
			t.Fatalf("UID FETCH THREADID = %q, want NIL", id)
		}
	}
	if !ids[eA] || !ids[eB] {
		t.Fatalf("UID FETCH EMAILIDs = %v, want %q and %q", ids, eA, eB)
	}

	// COPY preserves EMAILID across mailboxes despite a different UID.
	c.commandOK("CREATE dest")
	_, tagged = c.command("COPY 1 dest")
	if !copyUIDRe.MatchString(tagged) {
		t.Fatalf("COPY response missing COPYUID: %q", tagged)
	}
	untagged = c.commandOK("SELECT dest")
	untagged = c.commandOK("FETCH 1 (UID EMAILID)")
	line := strings.Join(untagged, "\n")
	if id := firstSubmatch(emailIDRe, line); id != eA {
		t.Fatalf("COPY dest EMAILID = %q, want %q", id, eA)
	}
	if uid := firstSubmatch(fetchUIDRe, line); uid != "1" {
		t.Fatalf("COPY dest UID = %q, want 1", uid)
	}

	// Two different messages with UID 1 in different mailboxes must not
	// share an identifier.
	c.commandOK("SELECT INBOX")
	c.commandOK("CREATE other")
	c.commandOK("UID COPY 2 other")
	untagged = c.commandOK("SELECT other")
	untagged = c.commandOK("FETCH 1 (UID EMAILID)")
	line = strings.Join(untagged, "\n")
	if uid := firstSubmatch(fetchUIDRe, line); uid != "1" {
		t.Fatalf("other UID = %q, want 1", uid)
	}
	if id := firstSubmatch(emailIDRe, line); id != eB {
		t.Fatalf("other EMAILID = %q, want %q", id, eB)
	}

	// MOVE preserves EMAILID and removes the source message.
	c.commandOK("CREATE mvbox")
	c.commandOK("SELECT INBOX")
	c.commandOK("MOVE 1 mvbox")
	untagged = c.commandOK("SELECT mvbox")
	untagged = c.commandOK("FETCH 1 (UID EMAILID)")
	if id := firstSubmatch(emailIDRe, strings.Join(untagged, "\n")); id != eA {
		t.Fatalf("MOVE dest EMAILID = %q, want %q", id, eA)
	}
	untagged = c.commandOK("SELECT INBOX")
	untagged = c.commandOK("FETCH 1:* (UID EMAILID)")
	if len(untagged) != 1 {
		t.Fatalf("INBOX has %d messages after MOVE, want 1: %v", len(untagged), untagged)
	}
	if id := firstSubmatch(emailIDRe, untagged[0]); id != eB {
		t.Fatalf("remaining INBOX EMAILID = %q, want %q", id, eB)
	}

	// SEARCH EMAILID: seq numbers for SEARCH, UIDs for UID SEARCH.
	untagged = c.commandOK("SEARCH EMAILID %s", eB)
	if got := searchResult(untagged); got != "1" {
		t.Fatalf("SEARCH EMAILID = %q, want 1", got)
	}
	untagged = c.commandOK("SEARCH EMAILID %s", eA)
	if got := searchResult(untagged); got != "" {
		t.Fatalf("SEARCH EMAILID for moved message = %q, want empty", got)
	}
	untagged = c.commandOK("UID SEARCH EMAILID %s", eB)
	if got := searchResult(untagged); got != "2" {
		t.Fatalf("UID SEARCH EMAILID = %q, want 2", got)
	}

	// Intersection with a UID criterion.
	untagged = c.commandOK("SEARCH UID 2 EMAILID %s", eB)
	if got := searchResult(untagged); got != "1" {
		t.Fatalf("SEARCH UID+EMAILID = %q, want 1", got)
	}
	untagged = c.commandOK("SEARCH UID 1 EMAILID %s", eB)
	if got := searchResult(untagged); got != "" {
		t.Fatalf("SEARCH non-matching UID+EMAILID = %q, want empty", got)
	}
	untagged = c.commandOK("UID SEARCH UID 2 EMAILID %s", eB)
	if got := searchResult(untagged); got != "2" {
		t.Fatalf("UID SEARCH UID+EMAILID = %q, want 2", got)
	}

	// Add a third message for NOT / OR combinations.
	msgC := rawMessage("Message C", "hello C")
	c.append("INBOX", msgC)
	untagged = c.commandOK("FETCH 1:* (EMAILID)")
	var eC string
	for _, l := range untagged {
		id := firstSubmatch(emailIDRe, l)
		if id == "" || id == eB {
			continue
		}
		eC = id
	}
	if eC == "" || eC == eB {
		t.Fatalf("bad third EMAILID: %q", eC)
	}

	untagged = c.commandOK("SEARCH NOT EMAILID %s", eB)
	if got := searchResult(untagged); got != "2" {
		t.Fatalf("SEARCH NOT EMAILID = %q, want 2", got)
	}
	untagged = c.commandOK("SEARCH OR EMAILID %s EMAILID %s", eB, eC)
	if got := searchResult(untagged); got != "1 2" {
		t.Fatalf("SEARCH OR EMAILID = %q, want \"1 2\"", got)
	}
	untagged = c.commandOK("SEARCH OR (EMAILID %s) (EMAILID %s)", eB, "doesnotexist")
	if got := searchResult(untagged); got != "1" {
		t.Fatalf("SEARCH OR with miss = %q, want 1", got)
	}

	// No threading support: THREADID searches never match, NOT matches all.
	untagged = c.commandOK("SEARCH THREADID T1")
	if got := searchResult(untagged); got != "" {
		t.Fatalf("SEARCH THREADID = %q, want empty", got)
	}
	untagged = c.commandOK("UID SEARCH THREADID %s", eB)
	if got := searchResult(untagged); got != "" {
		t.Fatalf("UID SEARCH THREADID = %q, want empty", got)
	}
	untagged = c.commandOK("SEARCH NOT THREADID T1")
	if got := searchResult(untagged); got != "1 2" {
		t.Fatalf("SEARCH NOT THREADID = %q, want \"1 2\"", got)
	}

	// Invalid requests are rejected with a tagged error and do not break
	// the command boundary.
	c.commandBad("SEARCH EMAILID bad!id")
	c.commandBad("SEARCH EMAILID")
	c.commandBad("FETCH 1 (EMAILID")
	untagged = c.commandOK("NOOP")
	_ = untagged
	untagged = c.commandOK("FETCH 1 (UID FLAGS RFC822.SIZE)")
	line = strings.Join(untagged, "\n")
	if !strings.Contains(line, "UID 2") || !strings.Contains(line, "RFC822.SIZE") || strings.Contains(line, "EMAILID") {
		t.Fatalf("ordinary FETCH altered: %q", line)
	}

	// STATUS on a non-existent mailbox is a clean tagged error.
	c.commandNo("STATUS nope (MAILBOXID)")
	c.commandOK("NOOP")
}

// TestServerWithoutObjectID checks that a server not advertising OBJECTID
// keeps plain IMAP4rev1/rev2 behavior.
func TestServerWithoutObjectID(t *testing.T) {
	_, addr := startObjectIDTestServer(t, false)
	c := dialRawIMAP(t, addr)
	defer c.close()

	untagged := c.commandOK("CAPABILITY")
	if capObjectRe.MatchString(strings.Join(untagged, "\n")) {
		t.Fatalf("OBJECTID must not be advertised: %v", untagged)
	}

	// CREATE/SELECT carry no MAILBOXID.
	_, tagged := c.command("CREATE plain")
	if !strings.Contains(tagged, " OK ") || objectIDRe.MatchString(tagged) {
		t.Fatalf("CREATE without OBJECTID = %q", tagged)
	}
	untagged = c.commandOK("SELECT INBOX")
	if objectIDRe.MatchString(strings.Join(untagged, "\n")) {
		t.Fatalf("SELECT without OBJECTID contains MAILBOXID: %v", untagged)
	}

	// OBJECTID-specific requests are rejected with a tagged error.
	c.commandBad("FETCH 1 (EMAILID)")
	c.commandBad("FETCH 1 (THREADID)")
	c.commandBad("STATUS INBOX (MAILBOXID)")
	c.commandBad("SEARCH EMAILID M1")

	// Ordinary operation still works.
	c.append("INBOX", rawMessage("Plain", "plain"))
	untagged = c.commandOK("FETCH 1:* (UID FLAGS RFC822.SIZE)")
	line := strings.Join(untagged, "\n")
	if !strings.Contains(line, "UID 1") || !strings.Contains(line, "RFC822.SIZE") {
		t.Fatalf("ordinary FETCH broken without OBJECTID: %q", line)
	}
	c.commandOK("UID SEARCH ALL")
	c.commandOK("NOOP")
}

// brokenObjectIDSession wraps the in-memory session and returns invalid
// OBJECTID values for specific mailboxes, simulating a backend failure.
type brokenObjectIDSession struct {
	imapserver.SessionIMAP4rev2
}

func (s *brokenObjectIDSession) CreateMailbox(name string, options *imap.CreateOptions) (*imap.CreateData, error) {
	if name == "brokenc" {
		return &imap.CreateData{MailboxID: "bad id"}, nil
	}
	return s.SessionIMAP4rev2.(imapserver.SessionObjectID).CreateMailbox(name, options)
}

func (s *brokenObjectIDSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.SessionIMAP4rev2.Select(name, options)
	if err == nil && name == "brokens" {
		data.MailboxID = ""
	}
	return data, err
}

func (s *brokenObjectIDSession) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	data, err := s.SessionIMAP4rev2.Status(name, options)
	if err == nil && name == "brokens" {
		data.MailboxID = ""
	}
	return data, err
}

// TestServerObjectIDBackendFailure verifies that invalid identifiers reported
// by the backend result in tagged error responses instead of forged success
// responses, without breaking the command boundary.
func TestServerObjectIDBackendFailure(t *testing.T) {
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(objectidTestUsername, objectidTestPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("User.Create(INBOX) = %v", err)
	}
	if err := user.Create("brokens", nil); err != nil {
		t.Fatalf("User.Create(brokens) = %v", err)
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &brokenObjectIDSession{
				SessionIMAP4rev2: memServer.NewSession().(imapserver.SessionIMAP4rev2),
			}, nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
			imap.CapObjectID:  {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()
	t.Cleanup(func() { server.Close() })

	c := dialRawIMAP(t, ln.Addr().String())
	defer c.close()

	// Invalid MAILBOXID on CREATE: tagged NO, no forged MAILBOXID.
	_, tagged := c.command("CREATE brokenc")
	if !strings.Contains(tagged, " NO ") || objectIDRe.MatchString(tagged) {
		t.Fatalf("CREATE with bad id = %q, want NO without MAILBOXID", tagged)
	}

	// Subsequent commands still work.
	_, tagged = c.command("CREATE good")
	if !strings.Contains(tagged, " OK ") || !objectIDRe.MatchString(tagged) {
		t.Fatalf("CREATE good after failure = %q", tagged)
	}

	// Missing MAILBOXID on STATUS: tagged NO.
	_, tagged = c.command("STATUS brokens (MAILBOXID)")
	if !strings.Contains(tagged, " NO ") {
		t.Fatalf("STATUS with bad id = %q, want NO", tagged)
	}
	c.commandOK("NOOP")

	// Missing MAILBOXID on SELECT: tagged NO, connection stays usable.
	_, tagged = c.command("SELECT brokens")
	if !strings.Contains(tagged, " NO ") {
		t.Fatalf("SELECT with bad id = %q, want NO", tagged)
	}
	c.commandOK("SELECT INBOX")
	c.commandOK("NOOP")
}

// searchResult extracts the numbers from a classic "* SEARCH ..." response.
func searchResult(untagged []string) string {
	for _, line := range untagged {
		if strings.HasPrefix(line, "* SEARCH ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "* SEARCH"))
		}
	}
	return ""
}
