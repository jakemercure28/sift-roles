package rejectionsync

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"job-search-automation/internal/db"
)

const (
	DefaultMailbox      = "[Gmail]/All Mail"
	TrashMailbox        = "[Gmail]/Trash"
	DefaultLookbackDays = 30
	DefaultMaxMessages  = 300
	DefaultTrashOverlap = 200
)

type Message struct {
	UID         uint32
	Subject     string
	FromAddress string
	MessageID   string
	ReceivedAt  string
	Raw         string
}

type FetchOptions struct {
	Mailbox      string
	LookbackDays int
	MaxMessages  int
	LastUID      *uint32
	UIDValidity  *string
	UIDOverlap   int
}

type FetchResult struct {
	UIDValidity string
	LastUID     *uint32
	Messages    []Message
}

type Fetcher interface {
	FetchMailboxMessages(ctx context.Context, opts FetchOptions) (FetchResult, error)
}

type Config struct {
	Email        string
	Password     string
	Mailbox      string
	LookbackDays int
	MaxMessages  int
	SkipTrash    bool
	DryRun       bool
	ClassifyOnly bool
	Replay       bool
	Fetcher      Fetcher
}

type Summary struct {
	Fetched    int `json:"fetched"`
	Candidates int `json:"candidates"`
	Applied    int `json:"applied"`
	DryRun     int `json:"dryRun"`
	Ignored    int `json:"ignored"`
	Unmatched  int `json:"unmatched"`
}

type syncState struct {
	LastUID     *uint32
	UIDValidity *string
}

type job struct {
	ID        string
	Company   string
	Title     string
	URL       string
	Stage     string
	Status    string
	AppliedAt string
}

type matchResult struct {
	Job        *job
	Confidence string
	Reason     string
}

func Sync(ctx context.Context, repo *db.Repository, cfg Config) (Summary, error) {
	if cfg.Fetcher == nil {
		if cfg.Email == "" || cfg.Password == "" {
			return Summary{}, errors.New("GMAIL_EMAIL or GMAIL_APP_PASSWORD not set")
		}
		cfg.Fetcher = GmailFetcher{Email: cfg.Email, Password: cfg.Password}
	}
	mailbox := cfg.Mailbox
	if mailbox == "" {
		mailbox = DefaultMailbox
	}
	main, err := sweepMailbox(ctx, repo, mailbox, "rejection_email", cfg, 0)
	if err != nil {
		return Summary{}, err
	}
	var trash Summary
	if !cfg.SkipTrash {
		trash, err = sweepMailbox(ctx, repo, TrashMailbox, "rejection_email_trash", cfg, DefaultTrashOverlap)
		if err != nil {
			return Summary{}, err
		}
	}
	main.add(trash)
	return main, nil
}

func (s *Summary) add(other Summary) {
	s.Fetched += other.Fetched
	s.Candidates += other.Candidates
	s.Applied += other.Applied
	s.DryRun += other.DryRun
	s.Ignored += other.Ignored
	s.Unmatched += other.Unmatched
}

func sweepMailbox(ctx context.Context, repo *db.Repository, mailbox, prefix string, cfg Config, uidOverlap int) (Summary, error) {
	state, err := loadState(repo, prefix)
	if err != nil {
		return Summary{}, err
	}
	lookback := cfg.LookbackDays
	if lookback <= 0 {
		lookback = DefaultLookbackDays
	}
	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = DefaultMaxMessages
	}
	var lastUID *uint32
	var uidValidity *string
	if !cfg.Replay {
		lastUID = state.LastUID
		uidValidity = state.UIDValidity
	}
	result, err := cfg.Fetcher.FetchMailboxMessages(ctx, FetchOptions{
		Mailbox:      mailbox,
		LookbackDays: lookback,
		MaxMessages:  maxMessages,
		LastUID:      lastUID,
		UIDValidity:  uidValidity,
		UIDOverlap:   uidOverlap,
	})
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{Fetched: len(result.Messages)}
	for _, message := range result.Messages {
		if !IsRejectionEmail(message) {
			continue
		}
		summary.Candidates++
		match, err := MatchRejectionEmail(repo, message)
		if err != nil {
			return Summary{}, err
		}
		if match.Job == nil {
			summary.Unmatched++
			if !cfg.ClassifyOnly {
				if err := logCandidateEmail(repo, mailbox, result.UIDValidity, message, match, "unmatched", match.Reason); err != nil {
					return Summary{}, err
				}
			}
			continue
		}
		applied, err := applyRejectionUpdate(repo, message, match, cfg.DryRun || cfg.ClassifyOnly)
		if err != nil {
			return Summary{}, err
		}
		switch applied.status {
		case "applied":
			summary.Applied++
		case "dry_run":
			summary.DryRun++
		case "ignored":
			summary.Ignored++
		default:
			summary.Unmatched++
		}
		if !cfg.ClassifyOnly {
			if err := logCandidateEmail(repo, mailbox, result.UIDValidity, message, match, applied.status, applied.reason); err != nil {
				return Summary{}, err
			}
		}
	}
	if !cfg.Replay && !cfg.ClassifyOnly {
		if err := saveState(repo, prefix, result.LastUID, result.UIDValidity); err != nil {
			return Summary{}, err
		}
	}
	return summary, nil
}

func loadState(repo *db.Repository, prefix string) (syncState, error) {
	raw, uid := repo.RawDB(), repo.UserID()
	var state syncState
	var last, validity sql.NullString
	if err := raw.QueryRow(repo.Rewrite("SELECT value FROM metadata WHERE key = ? AND user_id = ?"), prefix+"_last_uid", uid).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if err := raw.QueryRow(repo.Rewrite("SELECT value FROM metadata WHERE key = ? AND user_id = ?"), prefix+"_uid_validity", uid).Scan(&validity); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if last.Valid {
		if n, err := strconv.ParseUint(last.String, 10, 32); err == nil {
			v := uint32(n)
			state.LastUID = &v
		}
	}
	if validity.Valid {
		v := validity.String
		state.UIDValidity = &v
	}
	return state, nil
}

func saveState(repo *db.Repository, prefix string, lastUID *uint32, uidValidity string) error {
	raw, uid := repo.RawDB(), repo.UserID()
	const upsert = `
		INSERT INTO metadata (user_id, key, value, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(user_id, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
	`
	if lastUID != nil {
		if _, err := raw.Exec(repo.Rewrite(upsert), uid, prefix+"_last_uid", strconv.FormatUint(uint64(*lastUID), 10)); err != nil {
			return err
		}
	}
	if uidValidity != "" {
		if _, err := raw.Exec(repo.Rewrite(upsert), uid, prefix+"_uid_validity", uidValidity); err != nil {
			return err
		}
	}
	return nil
}

type applyResult struct {
	status string
	reason string
}

func applyRejectionUpdate(repo *db.Repository, message Message, match matchResult, dryRun bool) (applyResult, error) {
	raw, uid := repo.RawDB(), repo.UserID()
	var current struct {
		ID        string
		Stage     sql.NullString
		Status    sql.NullString
		AppliedAt sql.NullString
	}
	err := raw.QueryRow(repo.Rewrite(`SELECT id, stage, status, applied_at FROM jobs WHERE id = ? AND user_id = ?`), match.Job.ID, uid).
		Scan(&current.ID, &current.Stage, &current.Status, &current.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return applyResult{status: "unmatched", reason: "job_missing"}, nil
	}
	if err != nil {
		return applyResult{}, err
	}
	if current.Stage.String == "rejected" {
		return applyResult{status: "ignored", reason: "already_rejected"}, nil
	}
	if !current.AppliedAt.Valid || current.AppliedAt.String == "" {
		return applyResult{status: "ignored", reason: "job_not_applied"}, nil
	}
	if dryRun {
		return applyResult{status: "dry_run", reason: match.Reason}, nil
	}
	fromStage := current.Stage.String
	if fromStage == "" {
		fromStage = "applied"
	}
	rejectedAt := message.ReceivedAt
	if rejectedAt == "" {
		rejectedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	tx, err := raw.Begin()
	if err != nil {
		return applyResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(repo.Rewrite(`
		UPDATE jobs
		SET status = 'rejected',
			stage = 'rejected',
			rejected_from_stage = ?,
			rejected_at = COALESCE(rejected_at, ?),
			updated_at = datetime('now')
		WHERE id = ? AND user_id = ?
	`), fromStage, rejectedAt, current.ID, uid); err != nil {
		return applyResult{}, err
	}
	if _, err := tx.Exec(repo.Rewrite(`INSERT INTO events (user_id, job_id, event_type, from_value, to_value) VALUES (?, ?, 'stage_change', ?, 'rejected')`), uid, current.ID, fromStage); err != nil {
		return applyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return applyResult{}, err
	}
	return applyResult{status: "applied", reason: match.Reason}, nil
}

func logCandidateEmail(repo *db.Repository, mailbox, uidValidity string, message Message, match matchResult, status, reason string) error {
	var company, title, jobID, confidence any
	if match.Job != nil {
		company = match.Job.Company
		title = match.Job.Title
		jobID = match.Job.ID
		confidence = match.Confidence
	}
	// Conflict target is user-scoped (user_id, mailbox, uid_validity, uid): the
	// same Gmail UID belongs to a different tenant for a different user.
	_, err := repo.RawDB().Exec(repo.Rewrite(`
		INSERT INTO rejection_email_log (
			user_id, mailbox, uid_validity, uid, message_id, received_at, from_address,
			subject, company_hint, title_hint, matched_job_id, match_confidence,
			match_status, reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(user_id, mailbox, uid_validity, uid) DO UPDATE SET
			message_id=excluded.message_id,
			received_at=excluded.received_at,
			from_address=excluded.from_address,
			subject=excluded.subject,
			company_hint=excluded.company_hint,
			title_hint=excluded.title_hint,
			matched_job_id=excluded.matched_job_id,
			match_confidence=excluded.match_confidence,
			match_status=excluded.match_status,
			reason=excluded.reason,
			created_at=excluded.created_at
	`), repo.UserID(), mailbox, uidValidity, message.UID, nullable(message.MessageID), nullable(message.ReceivedAt), nullable(message.FromAddress), nullable(message.Subject), company, title, jobID, confidence, status, reason)
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var rejectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bunfortunately\b`),
	regexp.MustCompile(`(?i)\bregrettably\b`),
	regexp.MustCompile(`(?i)\bnot moving forward\b`),
	regexp.MustCompile(`(?i)\bwill not be moving forward\b`),
	regexp.MustCompile(`(?i)\bwon'?t be moving forward\b`),
	regexp.MustCompile(`(?i)\bdecided not to proceed\b`),
	regexp.MustCompile(`(?i)\bdecided to proceed with candidates?\b`),
	regexp.MustCompile(`(?i)\bdecided not to move forward\b`),
	regexp.MustCompile(`(?i)\bdecided to not move forward\b`),
	regexp.MustCompile(`(?i)\bdecided to move forward with others\b`),
	regexp.MustCompile(`(?i)\bhave decided not to move forward\b`),
	regexp.MustCompile(`(?i)\b(?:made|make) the decision to not move forward\b`),
	regexp.MustCompile(`(?i)\bnot proceed with your candidacy\b`),
	regexp.MustCompile(`(?i)\bextend an offer to another candidate\b`),
	regexp.MustCompile(`(?i)\bmove ahead with another candidate\b`),
	regexp.MustCompile(`(?i)\bmove forward with other candidates\b`),
	regexp.MustCompile(`(?i)\bmoving forward with other candidates\b`),
	regexp.MustCompile(`(?i)\bnot continuing with (?:any )?(?:new )?interviews\b`),
	regexp.MustCompile(`(?i)\bnot continuing with your application\b`),
	regexp.MustCompile(`(?i)\bbetter match for this (?:particular )?(?:position|role)\b`),
	regexp.MustCompile(`(?i)\bbackgrounds? more closely align\b`),
	regexp.MustCompile(`(?i)\b(?:position|role|job|opening|requisition)\s+(?:has|have)\s+(?:(?:now|recently|already)\s+)?been\s+(?:filled|closed|paused|put on hold|placed on hold)\b`),
	regexp.MustCompile(`(?i)\bno longer under consideration\b`),
	regexp.MustCompile(`(?i)\bnot selected\b`),
	regexp.MustCompile(`(?i)\bwe will not be proceeding\b`),
	regexp.MustCompile(`(?i)\bwe are unable to move forward\b`),
}

func IsRejectionEmail(message Message) bool {
	text := ReadableEmailText(message)
	for _, pattern := range rejectionPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func ReadableEmailText(message Message) string {
	decodedRaw := decodeQuotedPrintable(message.Raw)
	parts := extractMimeTextParts(message.Raw)
	stripped := stripHTML(decodedRaw, 50000)
	return strings.Join(nonEmpty([]string{message.Subject, message.FromAddress}, parts, []string{stripped}), "\n")
}

func nonEmpty(groups ...[]string) []string {
	var out []string
	for _, group := range groups {
		for _, v := range group {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func decodeQuotedPrintable(input string) string {
	normalized := regexp.MustCompile(`=\r?\n`).ReplaceAllString(input, "")
	var out strings.Builder
	var bytesBuf []byte
	flush := func() {
		if len(bytesBuf) > 0 {
			out.Write(bytesBuf)
			bytesBuf = nil
		}
	}
	for i := 0; i < len(normalized); i++ {
		if normalized[i] == '=' && i+2 < len(normalized) {
			if b, err := strconv.ParseUint(normalized[i+1:i+3], 16, 8); err == nil {
				bytesBuf = append(bytesBuf, byte(b))
				i += 2
				continue
			}
		}
		flush()
		out.WriteByte(normalized[i])
	}
	flush()
	return out.String()
}

func extractMimeTextParts(raw string) []string {
	sections := regexp.MustCompile(`(?m)\r?\n--[^\r\n]*`).Split(raw, -1)
	var out []string
	for _, section := range sections {
		if !regexp.MustCompile(`(?i)Content-Type:\s*text/(?:plain|html)\b`).MatchString(section) {
			continue
		}
		idx := regexp.MustCompile(`\r?\n\r?\n`).FindStringIndex(section)
		if idx == nil {
			continue
		}
		headers := section[:idx[0]]
		body := section[idx[1]:]
		contentType := getHeader(headers, "Content-Type")
		transfer := getHeader(headers, "Content-Transfer-Encoding")
		out = append(out, stripHTML(decodeMimeBody(body, transfer, charsetFromContentType(contentType)), 50000))
	}
	return out
}

func getHeader(headers, name string) string {
	re := regexp.MustCompile(`(?im)^[\t ]*` + regexp.QuoteMeta(name) + `:\s*([^\r\n]*(?:\r?\n[\t ][^\r\n]*)*)`)
	m := re.FindStringSubmatch(headers)
	if len(m) < 2 {
		return ""
	}
	return regexp.MustCompile(`\r?\n[\t ]+`).ReplaceAllString(m[1], " ")
}

func charsetFromContentType(contentType string) string {
	m := regexp.MustCompile(`(?i)\bcharset\s*=\s*"?([^";\s]+)"?`).FindStringSubmatch(contentType)
	if len(m) > 1 {
		return m[1]
	}
	return "utf-8"
}

func decodeMimeBody(body, transfer, charset string) string {
	switch strings.ToLower(transfer) {
	case "base64":
		cleaned := regexp.MustCompile(`(?i)[^a-z0-9+/=]`).ReplaceAllString(body, "")
		raw, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return body
		}
		return decodeCharset(raw, charset)
	case "quoted-printable":
		return decodeQuotedPrintable(body)
	default:
		return body
	}
}

func decodeCharset(raw []byte, charset string) string {
	switch strings.ToLower(strings.Trim(charset, `"'`)) {
	case "iso-8859-1", "latin1":
		runes := make([]rune, len(raw))
		for i, b := range raw {
			runes[i] = rune(b)
		}
		return string(runes)
	default:
		return string(raw)
	}
}

func stripHTML(text string, maxLen int) string {
	s := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(text, " ")
	s = htmlEntityDecode(s)
	s = regexp.MustCompile(`\s{2,}`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxLen {
		s = string([]rune(s)[:maxLen])
	}
	return s
}

func htmlEntityDecode(s string) string {
	replacer := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'")
	return replacer.Replace(s)
}

func NormalizeText(value string) string {
	s := strings.ToLower(value)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func normalizeCompact(value string) string {
	return strings.ReplaceAll(NormalizeText(value), " ", "")
}

func hasTerm(text, term string) bool {
	spacedHaystack := " " + NormalizeText(text) + " "
	spacedNeedle := " " + NormalizeText(term) + " "
	if strings.TrimSpace(spacedNeedle) != "" && strings.Contains(spacedHaystack, spacedNeedle) {
		return true
	}
	compactNeedle := normalizeCompact(term)
	return len(compactNeedle) >= 7 && strings.Contains(normalizeCompact(text), compactNeedle)
}

func extractLinks(text string) []string {
	matches := regexp.MustCompile(`https?://[^\s"'<>]+`).FindAllString(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		m = strings.TrimRight(m, "),.;")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

type parsedURL struct {
	Host  string
	Path  string
	JobID string
	Slug  string
	UUID  string
}

func parseMatchableURL(raw string) *parsedURL {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	p := &parsedURL{Host: strings.ToLower(u.Hostname()), Path: strings.ToLower(strings.Join(parts, "/"))}
	switch {
	case strings.Contains(p.Host, "greenhouse"):
		if len(parts) > 0 {
			p.Slug = parts[0]
		}
		p.JobID = u.Query().Get("gh_jid")
		for i, part := range parts {
			if part == "jobs" && i+1 < len(parts) && p.JobID == "" {
				p.JobID = parts[i+1]
			}
		}
	case strings.Contains(p.Host, "ashbyhq.com"), strings.Contains(p.Host, "lever.co"):
		if len(parts) > 0 {
			p.Slug = parts[0]
		}
		if len(parts) > 1 {
			p.UUID = parts[1]
		}
	case strings.Contains(p.Host, "ats.rippling.com"):
		if len(parts) > 0 {
			p.Slug = parts[0]
		}
		for i, part := range parts {
			if part == "jobs" && i+1 < len(parts) {
				p.UUID = parts[i+1]
			}
		}
	}
	return p
}

func MatchRejectionEmail(repo *db.Repository, message Message) (matchResult, error) {
	active, err := getAppliedJobs(repo, "AND COALESCE(stage, '') NOT IN ('rejected')")
	if err != nil {
		return matchResult{}, err
	}
	if m := matchByURL(message, active); m.Job != nil {
		return m, nil
	}
	result := matchByCompanyAndTitle(message, active)
	if result.Job != nil || result.Reason != "no_company_match" {
		return result, nil
	}
	rejected, err := getAppliedJobs(repo, "AND COALESCE(stage, '') = 'rejected'")
	if err != nil {
		return matchResult{}, err
	}
	if m := matchByURL(message, rejected); m.Job != nil {
		return m, nil
	}
	return matchByCompanyAndTitle(message, rejected), nil
}

func getAppliedJobs(repo *db.Repository, where string) ([]job, error) {
	rows, err := repo.RawDB().Query(repo.Rewrite(`
		SELECT id, company, title, url, stage, status, applied_at
		FROM jobs
		WHERE applied_at IS NOT NULL AND user_id = ? `+where), repo.UserID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		var company, title, url, stage, status, applied sql.NullString
		if err := rows.Scan(&j.ID, &company, &title, &url, &stage, &status, &applied); err != nil {
			return nil, err
		}
		j.Company, j.Title, j.URL = company.String, title.String, url.String
		j.Stage, j.Status, j.AppliedAt = stage.String, status.String, applied.String
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func matchByURL(message Message, jobs []job) matchResult {
	links := extractLinks(message.Raw + "\n" + message.Subject)
	if len(links) == 0 {
		return matchResult{}
	}
	var parsedLinks []*parsedURL
	for _, link := range links {
		if p := parseMatchableURL(link); p != nil {
			parsedLinks = append(parsedLinks, p)
		}
	}
	var candidates []job
	for _, j := range jobs {
		parsedJob := parseMatchableURL(j.URL)
		if parsedJob == nil {
			continue
		}
		for _, link := range parsedLinks {
			if parsedJob.Host != link.Host {
				continue
			}
			if parsedJob.JobID != "" && link.JobID != "" && parsedJob.JobID == link.JobID ||
				parsedJob.UUID != "" && link.UUID != "" && parsedJob.UUID == link.UUID ||
				parsedJob.Path != "" && link.Path != "" && parsedJob.Path == link.Path {
				candidates = append(candidates, j)
				break
			}
		}
	}
	if len(candidates) != 1 {
		return matchResult{}
	}
	return matchResult{Job: &candidates[0], Confidence: "strong", Reason: "url_match"}
}

func matchByCompanyAndTitle(message Message, jobs []job) matchResult {
	text := ReadableEmailText(message)
	companies := map[string][]job{}
	for _, j := range jobs {
		if !hasCompanyTerm(text, j.Company) {
			continue
		}
		key := NormalizeText(j.Company)
		companies[key] = append(companies[key], j)
	}
	if len(companies) == 0 {
		return matchResult{Confidence: "none", Reason: "no_company_match"}
	}
	if len(companies) > 1 {
		var all []job
		for _, bucket := range companies {
			all = append(all, bucket...)
		}
		titleMatches := filterTitleMatches(text, all)
		if title := uniquePreferredTitleMatch(titleMatches); title != nil {
			return matchResult{Job: title, Confidence: "strong", Reason: "company_title_match"}
		}
		if len(titleMatches) > 1 {
			return matchResult{Confidence: "none", Reason: "multiple_title_matches"}
		}
		return matchResult{Confidence: "none", Reason: "multiple_company_matches"}
	}
	var companyJobs []job
	for _, bucket := range companies {
		companyJobs = bucket
	}
	if len(companyJobs) == 1 {
		return matchResult{Job: &companyJobs[0], Confidence: "medium", Reason: "single_active_company_job"}
	}
	titleMatches := filterTitleMatches(text, companyJobs)
	if title := uniquePreferredTitleMatch(titleMatches); title != nil {
		return matchResult{Job: title, Confidence: "strong", Reason: "company_title_match"}
	}
	if len(titleMatches) > 1 {
		return matchResult{Confidence: "none", Reason: "multiple_title_matches"}
	}
	return matchResult{Confidence: "none", Reason: "ambiguous_company_match"}
}

func companyTerms(company string) []string {
	normalized := NormalizeText(company)
	terms := []string{company}
	compact := strings.ReplaceAll(normalized, " ", "")
	if strings.HasSuffix(compact, "data") && len(compact) > 8 {
		terms = append(terms, strings.TrimSuffix(compact, "data"))
	}
	return terms
}

func hasCompanyTerm(text, company string) bool {
	for _, term := range companyTerms(company) {
		if hasTerm(text, term) {
			return true
		}
	}
	return false
}

func filterTitleMatches(text string, jobs []job) []job {
	var out []job
	for _, j := range jobs {
		if hasTerm(text, j.Title) {
			out = append(out, j)
		}
	}
	return out
}

func uniquePreferredTitleMatch(matches []job) *job {
	if len(matches) == 1 {
		return &matches[0]
	}
	var nonArchived []job
	for _, j := range matches {
		if j.Status != "archived" {
			nonArchived = append(nonArchived, j)
		}
	}
	if len(nonArchived) == 1 {
		return &nonArchived[0]
	}
	return nil
}

type GmailFetcher struct {
	Email    string
	Password string
}

func (f GmailFetcher) FetchMailboxMessages(ctx context.Context, opts FetchOptions) (FetchResult, error) {
	imapClient, err := client.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		return FetchResult{}, err
	}
	defer imapClient.Logout()
	if err := imapClient.Login(f.Email, f.Password); err != nil {
		return FetchResult{}, err
	}
	mbox, err := imapClient.Select(firstNonEmpty(opts.Mailbox, DefaultMailbox), true)
	if err != nil {
		return FetchResult{}, err
	}
	uidValidity := fmt.Sprintf("%d", mbox.UidValidity)
	since := time.Now().AddDate(0, 0, -DefaultLookbackDays)
	if opts.LookbackDays > 0 {
		since = time.Now().AddDate(0, 0, -opts.LookbackDays)
	}
	criteria := imap.NewSearchCriteria()
	criteria.Since = since
	uids, err := imapClient.UidSearch(criteria)
	if err != nil {
		return FetchResult{}, err
	}
	if opts.UIDValidity != nil && *opts.UIDValidity == uidValidity && opts.LastUID != nil {
		floor := int64(*opts.LastUID) - int64(opts.UIDOverlap)
		if floor < 0 {
			floor = 0
		}
		var filtered []uint32
		for _, uid := range uids {
			if int64(uid) > floor {
				filtered = append(filtered, uid)
			}
		}
		uids = filtered
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	maxMessages := opts.MaxMessages
	if maxMessages <= 0 {
		maxMessages = DefaultMaxMessages
	}
	if len(uids) > maxMessages {
		uids = uids[len(uids)-maxMessages:]
	}
	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}
	if seqset.Empty() {
		return FetchResult{UIDValidity: uidValidity, LastUID: opts.LastUID}, nil
	}
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- imapClient.UidFetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, section.FetchItem()}, messages)
	}()
	var out []Message
	for msg := range messages {
		raw := ""
		if body := msg.GetBody(section); body != nil {
			data, _ := io.ReadAll(body)
			raw = string(data)
		}
		out = append(out, imapMessage(msg, raw))
	}
	if err := <-done; err != nil {
		return FetchResult{}, err
	}
	var last *uint32
	if len(uids) > 0 {
		v := uids[len(uids)-1]
		last = &v
	}
	return FetchResult{UIDValidity: uidValidity, LastUID: last, Messages: out}, ctx.Err()
}

func imapMessage(msg *imap.Message, raw string) Message {
	out := Message{UID: msg.Uid, Raw: raw}
	if msg.Envelope != nil {
		out.Subject = msg.Envelope.Subject
		out.MessageID = msg.Envelope.MessageId
		if !msg.Envelope.Date.IsZero() {
			out.ReceivedAt = msg.Envelope.Date.UTC().Format(time.RFC3339Nano)
		}
		var from []string
		for _, addr := range msg.Envelope.From {
			from = append(from, addr.Address())
		}
		out.FromAddress = strings.Join(from, ", ")
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
