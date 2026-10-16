package imap

// CreateOptions contains options for the CREATE command.
type CreateOptions struct {
	SpecialUse []MailboxAttr // requires CREATE-SPECIAL-USE
}

// CreateData is the data returned by a CREATE command.
type CreateData struct {
	// MailboxID is the server-allocated persistent identifier of the
	// mailbox. Requires OBJECTID.
	MailboxID string
}
