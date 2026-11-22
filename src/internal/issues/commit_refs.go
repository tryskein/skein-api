package issues

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// The length limits of the free fields of a commit ref, counted as Unicode
// code points like TitleMaxLength and LabelMaxLength. The url, author and
// message of a commit ref are written by whoever closes the issue or
// attaches the commit and are read back by every client of the card, so
// they are bounded and the url is held to a web link.
const (
	CommitURLMaxLength     = 2048
	CommitAuthorMaxLength  = 256
	CommitMessageMaxLength = 1000
)

// validateCommitRefs checks every commit ref of a close or an attach before
// any transaction opens: the hash is lowercase hex of 7..64 characters; the
// url, when given, is an absolute http or https URL with a host and at most
// CommitURLMaxLength characters; the author and the message, when given,
// are at most CommitAuthorMaxLength and CommitMessageMaxLength characters.
// A refusal is a ValidationError naming commits[i] and the field. Refs
// already stored are not re-checked.
func validateCommitRefs(commits []CloseCommitInput) error {
	for i, c := range commits {
		if !commitHashRegex.MatchString(c.Hash) {
			return &ValidationError{
				Code:    "invalid_commit_hash",
				Message: fmt.Sprintf("commits[%d].hash must be lowercase hex of length 7..64 (got %q)", i, c.Hash),
			}
		}
		if c.URL != nil {
			if err := validateCommitURL(i, *c.URL); err != nil {
				return err
			}
		}
		if c.Author != nil && utf8.RuneCountInString(*c.Author) > CommitAuthorMaxLength {
			return &ValidationError{
				Code:    "invalid_commit_author",
				Message: fmt.Sprintf("commits[%d].author must be at most %d characters", i, CommitAuthorMaxLength),
			}
		}
		if c.Message != nil && utf8.RuneCountInString(*c.Message) > CommitMessageMaxLength {
			return &ValidationError{
				Code:    "invalid_commit_message",
				Message: fmt.Sprintf("commits[%d].message must be at most %d characters", i, CommitMessageMaxLength),
			}
		}
	}
	return nil
}

// validateCommitURL accepts only an absolute http or https URL with a host:
// any other scheme (javascript:, data:, file:, …), a relative reference and
// an opaque form such as "http:example.com" are refused.
func validateCommitURL(i int, raw string) error {
	invalid := func(why string) error {
		return &ValidationError{
			Code:    "invalid_commit_url",
			Message: fmt.Sprintf("commits[%d].url %s", i, why),
		}
	}
	if utf8.RuneCountInString(raw) > CommitURLMaxLength {
		return invalid(fmt.Sprintf("must be at most %d characters", CommitURLMaxLength))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("is not a valid URL")
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return invalid("must be an absolute http or https URL")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return invalid("must be an absolute http or https URL")
	}
	return nil
}
