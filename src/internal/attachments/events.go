package attachments

// Event-type constants for the attachment domain. Subjects on NATS
// follow the standard `skein.events.attachment.{action}.project.{pid}.{issueID}`
// shape produced by outbox.BuildSubject.
//
// `attachment.added` is in the notifiable set (R4.2 plan G-8): when an
// attachment is uploaded to an issue, every issue/project watcher gets
// an inbox row.
//
// `attachment.removed` is NOT in the notifiable set — deletes are
// expected to be common UI maintenance and noisy notifications would
// erode the inbox signal-to-noise ratio.
const (
	EventAttachmentAdded   = "attachment.added"
	EventAttachmentRemoved = "attachment.removed"
)
