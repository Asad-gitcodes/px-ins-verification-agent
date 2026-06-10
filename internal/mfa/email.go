package mfa

import (
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type EmailConfig struct {
	// Password is runtime-only plaintext. Snapshots store encrypted envelopes;
	// jobmgr decrypts into this shape immediately before a payer session.
	Host            string
	Port            int
	Secure          bool
	User            string
	Password        string
	ExpectedTo      string
	DeleteAfterRead bool
	CleanupMailbox  string
	Mailbox         string
	TimeoutMS       int
	PollIntervalMS  int
}

var (
	codeInsteadPattern  = regexp.MustCompile(`(?i)enter a code instead:\s*(\d{6})`)
	sixDigitCodePattern = regexp.MustCompile(`\b(\d{6})\b`)
)

const messageFreshnessSkew = 2 * time.Minute

func GetEmailCode(cfg EmailConfig, after time.Time) (string, error) {
	if cfg.Host == "" {
		return "", fmt.Errorf("mfa email host is required")
	}
	if cfg.User == "" {
		return "", fmt.Errorf("mfa email user is required")
	}
	if cfg.Password == "" {
		return "", fmt.Errorf("mfa email password is required")
	}

	timeout := durationFromMillis(cfg.TimeoutMS, 60*time.Second)
	pollInterval := durationFromMillis(cfg.PollIntervalMS, 3*time.Second)
	deadline := time.Now().Add(timeout)

	for {
		code, err := pollMailboxOnce(cfg, after)
		if err != nil {
			return "", err
		}
		if code != "" {
			return code, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for MFA email code")
		}
		time.Sleep(pollInterval)
	}
}

func pollMailboxOnce(cfg EmailConfig, after time.Time) (string, error) {
	imapClient, err := dial(cfg)
	if err != nil {
		return "", fmt.Errorf("connect to MFA mailbox: %w", err)
	}

	if err := imapClient.Login(cfg.User, cfg.Password); err != nil {
		_ = imapClient.Logout()
		return "", fmt.Errorf("login to MFA mailbox: %w", err)
	}
	defer imapClient.Logout()

	mailbox := cfg.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := imapClient.Select(mailbox, !cfg.DeleteAfterRead); err != nil {
		return "", fmt.Errorf("select MFA mailbox %q: %w", mailbox, err)
	}

	// IMAP SEARCH only uses the date portion, so fetch candidates from just
	// before the browser requested MFA and then filter precisely in Go.
	searchDate := after.Add(-1 * time.Minute)
	criteria := imap.NewSearchCriteria()
	criteria.Since = searchDate
	uids, err := imapClient.UidSearch(criteria)
	if err != nil {
		return "", fmt.Errorf("search MFA mailbox: %w", err)
	}

	sort.Slice(uids, func(i, j int) bool {
		return uids[i] > uids[j]
	})

	const maxMessagesToInspect = 50
	if len(uids) > maxMessagesToInspect {
		uids = uids[:maxMessagesToInspect]
	}

	bodySection := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchUid, bodySection.FetchItem()}

	for _, uid := range uids {
		seqSet := new(imap.SeqSet)
		seqSet.AddNum(uid)

		messages := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() {
			done <- imapClient.UidFetch(seqSet, items, messages)
		}()

		for msg := range messages {
			if msg == nil {
				continue
			}
			if code, err := codeFromMessage(msg, bodySection, cfg.ExpectedTo, after); err != nil {
				return "", err
			} else if code != "" {
				if cfg.DeleteAfterRead {
					cleanupMFAMessages(imapClient, uids, bodySection, cfg.ExpectedTo, cfg.CleanupMailbox)
				}
				return code, nil
			}
		}
		if err := <-done; err != nil {
			return "", fmt.Errorf("fetch MFA message: %w", err)
		}
	}

	return "", nil
}

func codeFromMessage(msg *imap.Message, bodySection *imap.BodySectionName, expectedTo string, after time.Time) (string, error) {
	messageDate := msg.InternalDate
	if msg.Envelope != nil && !msg.Envelope.Date.IsZero() {
		messageDate = msg.Envelope.Date
	}
	if messageDate.Before(after.Add(-messageFreshnessSkew)) {
		log.Printf("[MFA] skipped old verification email date=%s expectedTo=%q", messageDate.Format(time.RFC3339), expectedTo)
		return "", nil
	}
	if expectedTo != "" && !messageMatchesTo(msg, expectedTo) {
		log.Printf("[MFA] skipped verification email with different recipient date=%s expectedTo=%q actualTo=%q", messageDate.Format(time.RFC3339), expectedTo, messageToAddresses(msg))
		return "", nil
	}

	code, err := codeFromMatchingMessage(msg, bodySection)
	if err != nil {
		return "", err
	}
	if code != "" {
		log.Printf("[MFA] found matching verification email date=%s to=%q", messageDate.Format(time.RFC3339), expectedTo)
	}
	return code, nil
}

func codeFromMatchingMessage(msg *imap.Message, bodySection *imap.BodySectionName) (string, error) {
	subject := ""
	if msg.Envelope != nil {
		subject = msg.Envelope.Subject
	}

	var body string
	if reader := msg.GetBody(bodySection); reader != nil {
		bodyBytes, err := io.ReadAll(reader)
		if err != nil {
			return "", fmt.Errorf("read MFA message body: %w", err)
		}
		body = string(bodyBytes)
	}

	return extractSixDigitCode(subject + "\n" + body), nil
}

func messageMatchesTo(msg *imap.Message, expectedTo string) bool {
	if msg.Envelope == nil {
		return false
	}

	expectedTo = strings.ToLower(strings.TrimSpace(expectedTo))
	for _, addr := range msg.Envelope.To {
		if strings.EqualFold(addr.Address(), expectedTo) {
			return true
		}
	}
	return false
}

func messageToAddresses(msg *imap.Message) []string {
	if msg.Envelope == nil {
		return nil
	}

	addresses := make([]string, 0, len(msg.Envelope.To))
	for _, addr := range msg.Envelope.To {
		addresses = append(addresses, strings.ToLower(addr.Address()))
	}
	return addresses
}

func cleanupMFAMessages(imapClient *client.Client, candidateUIDs []uint32, bodySection *imap.BodySectionName, expectedTo string, cleanupMailbox string) {
	var deleteUIDs []uint32
	for _, uid := range candidateUIDs {
		msg, err := fetchMessageByUID(imapClient, uid, bodySection)
		if err != nil {
			log.Printf("[MFA] could not inspect verification email uid=%d for cleanup: %v", uid, err)
			continue
		}
		if expectedTo != "" && !messageMatchesTo(msg, expectedTo) {
			continue
		}
		code, err := codeFromMatchingMessage(msg, bodySection)
		if err != nil {
			log.Printf("[MFA] could not read verification email uid=%d for cleanup: %v", uid, err)
			continue
		}
		if code != "" {
			deleteUIDs = append(deleteUIDs, uid)
		}
	}
	if len(deleteUIDs) == 0 {
		return
	}

	if cleanupMailbox == "" {
		cleanupMailbox = "[Gmail]/Trash"
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(deleteUIDs...)
	if err := imapClient.UidMove(seqSet, cleanupMailbox); err != nil {
		log.Printf("[MFA] read verification code but could not move %d email(s) to %q: %v", len(deleteUIDs), cleanupMailbox, err)
		return
	}
	log.Printf("[MFA] moved %d verification email(s) to %q", len(deleteUIDs), cleanupMailbox)
}

func fetchMessageByUID(imapClient *client.Client, uid uint32, bodySection *imap.BodySectionName) (*imap.Message, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchUid, bodySection.FetchItem()}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- imapClient.UidFetch(seqSet, items, messages)
	}()

	var found *imap.Message
	for msg := range messages {
		if msg != nil {
			found = msg
		}
	}
	if err := <-done; err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("message not found")
	}
	return found, nil
}

func dial(cfg EmailConfig) (*client.Client, error) {
	port := cfg.Port
	if port == 0 {
		if cfg.Secure {
			port = 993
		} else {
			port = 143
		}
	}

	address := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", port))
	if cfg.Secure {
		return client.DialTLS(address, nil)
	}
	return client.Dial(address)
}

func extractSixDigitCode(text string) string {
	if match := codeInsteadPattern.FindStringSubmatch(text); len(match) >= 2 {
		return match[1]
	}
	match := sixDigitCodePattern.FindStringSubmatch(strings.TrimSpace(text))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func durationFromMillis(value int, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}
