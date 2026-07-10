package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type TicketStatus string

const (
	StatusOpen       TicketStatus = "open"
	StatusInProgress TicketStatus = "inprogress"
	StatusCodeReview TicketStatus = "codereview"
	StatusTest       TicketStatus = "test"
	StatusVerified   TicketStatus = "verified"
	StatusClosed     TicketStatus = "closed"
)

var statusOrder = []TicketStatus{
	StatusOpen,
	StatusInProgress,
	StatusCodeReview,
	StatusTest,
	StatusVerified,
	StatusClosed,
}

var statusLabels = map[TicketStatus]string{
	StatusOpen:       "Open",
	StatusInProgress: "In Progress",
	StatusCodeReview: "Code Review",
	StatusTest:       "Test",
	StatusVerified:   "Verified",
	StatusClosed:     "Closed",
}

var validTypes = map[string]bool{
	"story": true,
	"bug":   true,
	"task":  true,
}

var (
	storyPointsCompleted = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scrum_story_points_completed_total",
		Help: "Current number of closed story points.",
	})
	activeBugs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scrum_active_bugs_count",
		Help: "Current number of active bugs in non-closed states.",
	})
	sprintVelocity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scrum_sprint_velocity",
		Help: "Current sprint velocity as closed story points.",
	})
)

func init() {
	prometheus.MustRegister(storyPointsCompleted)
	prometheus.MustRegister(activeBugs)
	prometheus.MustRegister(sprintVelocity)
}

type Ticket struct {
	ID          string
	Title       string
	Description string
	Assignee    string
	Type        string
	StoryPoints int
	Status      TicketStatus
}

type StatusChoice struct {
	Key   TicketStatus
	Label string
}

type TicketView struct {
	Ticket
	AllowedMoves []StatusChoice
}

type StatusColumn struct {
	Key     TicketStatus
	Label   string
	Tickets []TicketView
}

type DashboardData struct {
	Columns           []StatusColumn
	TotalTickets      int
	InFlightTickets   int
	ClosedTickets     int
	CompletedPoints   int
	ActiveBugs        int
	CompletionPercent int
	CurrentVelocity   int

	NextTicketIDPreview string
	FilterAssignee      string
	FilterType          string
	AssigneeOptions     []string
	TypeOptions         []string
	EditTicketID        string
}

type DashboardFilter struct {
	Assignee string
	Type     string
}

type Store struct {
	db *sql.DB
}

func newStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := openDBWithRetry(ctx, dsn)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	if err := s.seedIfEmpty(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func openDBWithRetry(ctx context.Context, dsn string) (*sql.DB, error) {
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error

	for time.Now().Before(deadline) {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			lastErr = err
			time.Sleep(1500 * time.Millisecond)
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return db, nil
		}
		_ = db.Close()
		lastErr = err
		time.Sleep(1500 * time.Millisecond)
	}

	if lastErr == nil {
		lastErr = errors.New("failed to connect to postgres")
	}
	return nil, lastErr
}

func (s *Store) migrate(ctx context.Context) error {
	const query = `
	CREATE TABLE IF NOT EXISTS tickets (
		ticket_key TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		assignee TEXT NOT NULL DEFAULT '',
		ticket_type TEXT NOT NULL CHECK (ticket_type IN ('story', 'bug', 'task')),
		story_points INT NOT NULL CHECK (story_points >= 1 AND story_points <= 21),
		status TEXT NOT NULL CHECK (status IN ('open', 'inprogress', 'codereview', 'test', 'verified', 'closed')),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets(status);
	CREATE INDEX IF NOT EXISTS idx_tickets_assignee ON tickets(assignee);
	CREATE INDEX IF NOT EXISTS idx_tickets_type ON tickets(ticket_type);
	`
	_, err := s.db.ExecContext(ctx, query)
	return err
}

func (s *Store) seedIfEmpty(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	seed := []Ticket{
		{ID: "SCRUM-1001", Title: "Set up sprint backlog view", Description: "Show epics and stories by priority.", Assignee: "Nora", Type: "story", StoryPoints: 5, Status: StatusOpen},
		{ID: "SCRUM-1002", Title: "Fix login session timeout", Description: "Users should stay signed in for 8 hours.", Assignee: "Jay", Type: "bug", StoryPoints: 3, Status: StatusInProgress},
		{ID: "SCRUM-1003", Title: "Improve chart loading speed", Description: "Dashboard chart should render under 1.5s.", Assignee: "Mira", Type: "task", StoryPoints: 2, Status: StatusCodeReview},
	}

	for _, t := range seed {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO tickets (ticket_key, title, description, assignee, ticket_type, story_points, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, t.ID, t.Title, t.Description, t.Assignee, t.Type, t.StoryPoints, string(t.Status))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) nextTicketNumber(ctx context.Context) (int, error) {
	var n int
	const q = `
	SELECT COALESCE(MAX(CAST(SPLIT_PART(ticket_key, '-', 2) AS INT)), 1000) + 1
	FROM tickets
	WHERE ticket_key LIKE 'SCRUM-%'
	`
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) CreateTicket(ctx context.Context, title, description, assignee, ticketType string, storyPoints int) error {
	nextNum, err := s.nextTicketNumber(ctx)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("SCRUM-%d", nextNum)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tickets (ticket_key, title, description, assignee, ticket_type, story_points, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, title, description, assignee, ticketType, storyPoints, string(StatusOpen))
	return err
}

func (s *Store) UpdateTicket(ctx context.Context, id, title, description, assignee, ticketType string, storyPoints int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tickets
		SET title = $1,
			description = $2,
			assignee = $3,
			ticket_type = $4,
			story_points = $5,
			updated_at = NOW()
		WHERE ticket_key = $6
	`, title, description, assignee, ticketType, storyPoints, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteTicket(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tickets WHERE ticket_key = $1`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MoveTicket(ctx context.Context, id string, next TicketStatus) error {
	if !isValidStatus(next) {
		return errors.New("invalid status")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tickets WHERE ticket_key = $1 FOR UPDATE`, id).Scan(&current); err != nil {
		return err
	}

	if !isAllowedTransition(TicketStatus(current), next) {
		return errors.New("only next or previous stage transitions are allowed")
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE tickets
		SET status = $1,
			updated_at = NOW()
		WHERE ticket_key = $2
	`, string(next), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	return tx.Commit()
}

func (s *Store) listTickets(ctx context.Context, filter DashboardFilter) ([]Ticket, error) {
	query := `
		SELECT ticket_key, title, description, assignee, ticket_type, story_points, status
		FROM tickets
		WHERE 1=1
	`
	args := []any{}
	argIdx := 1

	if filter.Assignee != "" {
		query += fmt.Sprintf(" AND assignee = $%d", argIdx)
		args = append(args, filter.Assignee)
		argIdx++
	}
	if filter.Type != "" {
		query += fmt.Sprintf(" AND ticket_type = $%d", argIdx)
		args = append(args, filter.Type)
		argIdx++
	}

	query += ` ORDER BY ticket_key ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []Ticket{}
	for rows.Next() {
		var t Ticket
		var status string
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.Assignee, &t.Type, &t.StoryPoints, &status); err != nil {
			return nil, err
		}
		t.Status = TicketStatus(status)
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) assigneeOptions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT assignee
		FROM tickets
		WHERE assignee <> ''
		ORDER BY assignee ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var assignee string
		if err := rows.Scan(&assignee); err != nil {
			return nil, err
		}
		out = append(out, assignee)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) refreshPrometheus(ctx context.Context) error {
	var bugCount int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tickets
		WHERE ticket_type = 'bug' AND status <> 'closed'
	`).Scan(&bugCount)
	if err != nil {
		return err
	}

	var closedSP int
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(story_points), 0)
		FROM tickets
		WHERE status = 'closed'
	`).Scan(&closedSP)
	if err != nil {
		return err
	}

	activeBugs.Set(float64(bugCount))
	sprintVelocity.Set(float64(closedSP))
	storyPointsCompleted.Set(float64(closedSP))
	return nil
}

func (s *Store) DashboardSnapshot(ctx context.Context, filter DashboardFilter, editTicketID string) (DashboardData, error) {
	tickets, err := s.listTickets(ctx, filter)
	if err != nil {
		return DashboardData{}, err
	}
	assignees, err := s.assigneeOptions(ctx)
	if err != nil {
		return DashboardData{}, err
	}

	columns := []StatusColumn{
		{Key: StatusOpen, Label: statusLabels[StatusOpen]},
		{Key: StatusInProgress, Label: statusLabels[StatusInProgress]},
		{Key: StatusCodeReview, Label: statusLabels[StatusCodeReview]},
		{Key: StatusTest, Label: statusLabels[StatusTest]},
		{Key: StatusVerified, Label: statusLabels[StatusVerified]},
		{Key: StatusClosed, Label: statusLabels[StatusClosed]},
	}
	idx := map[TicketStatus]int{}
	for i, c := range columns {
		idx[c.Key] = i
	}

	var inFlight int
	var closed int
	var totalSP int
	var closedSP int
	var bugs int

	for _, t := range tickets {
		col := idx[t.Status]
		columns[col].Tickets = append(columns[col].Tickets, TicketView{
			Ticket:        t,
			AllowedMoves:  allowedMoves(t.Status),
		})

		totalSP += t.StoryPoints
		if t.Status != StatusOpen && t.Status != StatusClosed {
			inFlight++
		}
		if t.Status == StatusClosed {
			closed++
			closedSP += t.StoryPoints
		}
		if t.Type == "bug" && t.Status != StatusClosed {
			bugs++
		}
	}

	for i := range columns {
		sort.Slice(columns[i].Tickets, func(a, b int) bool {
			return columns[i].Tickets[a].ID < columns[i].Tickets[b].ID
		})
	}

	nextNum, err := s.nextTicketNumber(ctx)
	if err != nil {
		return DashboardData{}, err
	}

	completion := 0
	if totalSP > 0 {
		completion = int(float64(closedSP) / float64(totalSP) * 100)
	}

	if err := s.refreshPrometheus(ctx); err != nil {
		log.Printf("metric refresh error: %v", err)
	}

	return DashboardData{
		Columns:             columns,
		TotalTickets:        len(tickets),
		InFlightTickets:     inFlight,
		ClosedTickets:       closed,
		CompletedPoints:     closedSP,
		ActiveBugs:          bugs,
		CompletionPercent:   completion,
		CurrentVelocity:     closedSP,
		NextTicketIDPreview: fmt.Sprintf("SCRUM-%d", nextNum),
		FilterAssignee:      filter.Assignee,
		FilterType:          filter.Type,
		AssigneeOptions:     assignees,
		TypeOptions:         []string{"story", "bug", "task"},
		EditTicketID:        editTicketID,
	}, nil
}

func normalizeTicketType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if validTypes[v] {
		return v
	}
	return "task"
}

func isValidStatus(status TicketStatus) bool {
	for _, s := range statusOrder {
		if s == status {
			return true
		}
	}
	return false
}

func statusIndex(status TicketStatus) int {
	for i, s := range statusOrder {
		if s == status {
			return i
		}
	}
	return -1
}

func isAllowedTransition(from, to TicketStatus) bool {
	fromIdx := statusIndex(from)
	toIdx := statusIndex(to)
	if fromIdx < 0 || toIdx < 0 || from == to {
		return false
	}
	delta := fromIdx - toIdx
	if delta < 0 {
		delta = -delta
	}
	return delta == 1
}

func allowedMoves(current TicketStatus) []StatusChoice {
	i := statusIndex(current)
	if i < 0 {
		return nil
	}

	moves := []StatusChoice{}
	if i-1 >= 0 {
		prev := statusOrder[i-1]
		moves = append(moves, StatusChoice{Key: prev, Label: statusLabels[prev]})
	}
	if i+1 < len(statusOrder) {
		next := statusOrder[i+1]
		moves = append(moves, StatusChoice{Key: next, Label: statusLabels[next]})
	}
	return moves
}

func parseFilter(r *http.Request) DashboardFilter {
	return DashboardFilter{
		Assignee: strings.TrimSpace(r.URL.Query().Get("assignee")),
		Type:     normalizeFilterType(r.URL.Query().Get("type")),
	}
}

func normalizeFilterType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	if validTypes[v] {
		return v
	}
	return ""
}

func parseReturnFilter(r *http.Request) DashboardFilter {
	return DashboardFilter{
		Assignee: strings.TrimSpace(r.FormValue("filter_assignee")),
		Type:     normalizeFilterType(r.FormValue("filter_type")),
	}
}

func redirectDashboard(w http.ResponseWriter, r *http.Request, filter DashboardFilter, editID string) {
	q := url.Values{}
	if filter.Assignee != "" {
		q.Set("assignee", filter.Assignee)
	}
	if filter.Type != "" {
		q.Set("type", filter.Type)
	}
	if editID != "" {
		q.Set("edit", editID)
	}

	target := "/"
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func parseTicketAction(path string) (ticketID string, action string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "tickets" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func mustTemplate() *template.Template {
	const page = `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>Agile Delivery Board</title>
	<link rel="preconnect" href="https://fonts.googleapis.com" />
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
	<link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@400;500;700&family=DM+Mono:wght@400;500&display=swap" rel="stylesheet" />
	<style>
		:root {
			--ink: #15223a;
			--card: #ffffff;
			--paper: #f2efe8;
			--brand: #ff6f3c;
			--brand-dark: #d35026;
			--line: #d6d1c5;
			--danger: #b43030;
			--shadow: 0 16px 36px rgba(21, 34, 58, 0.12);
		}
		* { box-sizing: border-box; }
		body {
			margin: 0;
			color: var(--ink);
			font-family: "Space Grotesk", "Avenir Next", "Segoe UI", sans-serif;
			background:
				radial-gradient(1200px 500px at -10% -10%, #ffd4c4 0%, rgba(255, 212, 196, 0) 60%),
				radial-gradient(900px 400px at 120% 0%, #d6efe1 0%, rgba(214, 239, 225, 0) 50%),
				var(--paper);
			min-height: 100vh;
		}
		.shell {
			width: min(1450px, 95vw);
			margin: 0 auto;
			padding: 28px 0 42px;
		}
		.hero {
			background: linear-gradient(140deg, #0f2745 0%, #1e4465 55%, #2f5d71 100%);
			color: #fff;
			border-radius: 22px;
			padding: 28px;
			box-shadow: var(--shadow);
			display: grid;
			gap: 16px;
			grid-template-columns: 1.7fr 1fr;
		}
		.hero h1 { margin: 0; font-size: clamp(1.6rem, 2.8vw, 2.5rem); }
		.hero p { margin: 8px 0 0; opacity: 0.94; }
		.metric-strip { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; }
		.metric { background: rgba(255,255,255,0.1); border-radius: 12px; padding: 12px; }
		.metric .value { font-size: 1.3rem; font-weight: 700; }
		.metric .label { font-size: 0.82rem; opacity: 0.85; margin-top: 4px; }

		.panel {
			margin-top: 16px;
			background: var(--card);
			border: 1px solid var(--line);
			border-radius: 16px;
			padding: 14px;
			box-shadow: var(--shadow);
		}
		.panel h2 { margin: 0 0 10px; font-size: 1.06rem; }
		.grid { display: grid; gap: 10px; grid-template-columns: repeat(5, minmax(120px, 1fr)); }
		.grid input, .grid select, textarea {
			width: 100%; border-radius: 10px; border: 1px solid var(--line);
			padding: 10px; font-family: inherit; background: #fff;
		}
		textarea { margin-top: 10px; min-height: 70px; resize: vertical; }
		.controls { display: flex; gap: 10px; flex-wrap: wrap; }
		.btn {
			border: none; border-radius: 10px; padding: 9px 14px; font-weight: 700;
			cursor: pointer; font-family: inherit;
		}
		.btn-main { background: var(--brand); color: #fff; }
		.btn-main:hover { background: var(--brand-dark); }
		.btn-dark { background: #1d3557; color: #fff; }
		.btn-danger { background: var(--danger); color: #fff; }
		.link-btn {
			display: inline-block; background: #355070; color: #fff; text-decoration: none;
			border-radius: 8px; padding: 7px 10px; font-size: 0.8rem;
		}
		.board {
			margin-top: 18px; display: grid; gap: 12px;
			grid-template-columns: repeat(6, minmax(230px, 1fr));
			overflow-x: auto; padding-bottom: 6px;
		}
		.column {
			background: rgba(255,255,255,0.72); border: 1px solid var(--line);
			border-radius: 16px; padding: 12px; min-height: 280px;
		}
		.column h3 { margin: 0; font-size: 1rem; }
		.count { margin-top: 5px; color: #4a5a74; font-size: 0.83rem; }
		.ticket {
			margin-top: 10px; background: #fff; border: 1px solid #e8e2d7;
			border-radius: 12px; padding: 10px; box-shadow: 0 6px 12px rgba(33,43,56,0.08);
		}
		.tag { font-family: "DM Mono", monospace; font-size: 0.75rem; color: #505d74; }
		.ticket h4 { margin: 8px 0 6px; font-size: 0.97rem; }
		.ticket p { margin: 0; font-size: 0.84rem; color: #3f4c62; }
		.meta {
			margin-top: 8px; font-size: 0.78rem; color: #5f6f86;
			display: flex; justify-content: space-between; gap: 6px;
		}
		.actions { margin-top: 8px; display: grid; gap: 8px; }
		.move { display: flex; gap: 6px; align-items: center; }
		.move select {
			flex: 1; min-width: 0; border: 1px solid var(--line); border-radius: 8px;
			padding: 7px; font-size: 0.84rem; background: #fff; font-family: inherit;
		}
		.small-row { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
		.small-row .btn, .small-row .link-btn { font-size: 0.76rem; padding: 7px 9px; }
		.edit-form { margin-top: 8px; border-top: 1px solid #ebe4d8; padding-top: 8px; }
		.edit-form input, .edit-form select, .edit-form textarea {
			margin-top: 6px; width: 100%; border: 1px solid var(--line); border-radius: 8px; padding: 7px;
		}
		.empty {
			margin-top: 12px; color: #6e7a8d; font-size: 0.85rem; text-align: center;
			border: 1px dashed #c8c1b3; border-radius: 10px; padding: 10px 8px;
			background: #fffcf7;
		}
		.footer {
			margin-top: 16px; font-size: 0.85rem; color: #41526a;
			display: flex; justify-content: space-between; gap: 10px; flex-wrap: wrap;
		}
		@media (max-width: 1080px) {
			.hero { grid-template-columns: 1fr; }
			.grid { grid-template-columns: repeat(2, minmax(120px, 1fr)); }
			.board { grid-template-columns: repeat(6, 270px); }
		}
		@media (max-width: 640px) {
			.shell { width: 96vw; }
			.metric-strip { grid-template-columns: 1fr 1fr; }
			.grid { grid-template-columns: 1fr; }
		}
	</style>
</head>
<body>
	<main class="shell">
		<section class="hero">
			<div>
				<h1>Agile Delivery Board</h1>
				<p>Create, edit, filter, and move tickets through Open → In Progress → Code Review → Test → Verified → Closed.</p>
			</div>
			<div class="metric-strip">
				<article class="metric"><div class="value">{{.TotalTickets}}</div><div class="label">Visible Tickets</div></article>
				<article class="metric"><div class="value">{{.InFlightTickets}}</div><div class="label">In Progress</div></article>
				<article class="metric"><div class="value">{{.ClosedTickets}}</div><div class="label">Closed</div></article>
				<article class="metric"><div class="value">{{.CompletionPercent}}%</div><div class="label">Completion</div></article>
				<article class="metric"><div class="value">{{.CurrentVelocity}} SP</div><div class="label">Sprint Velocity</div></article>
				<article class="metric"><div class="value">{{.ActiveBugs}}</div><div class="label">Active Bugs</div></article>
			</div>
		</section>

		<section class="panel">
			<h2>Filters</h2>
			<form class="controls" method="get" action="/">
				<select name="assignee">
					<option value="">All assignees</option>
					{{range .AssigneeOptions}}
					<option value="{{.}}" {{if eq $.FilterAssignee .}}selected{{end}}>{{.}}</option>
					{{end}}
				</select>
				<select name="type">
					<option value="">All types</option>
					{{range .TypeOptions}}
					<option value="{{.}}" {{if eq $.FilterType .}}selected{{end}}>{{.}}</option>
					{{end}}
				</select>
				<button class="btn btn-dark" type="submit">Apply</button>
				<a class="link-btn" href="/">Reset</a>
			</form>
		</section>

		<section class="panel">
			<h2>Create Ticket <span class="tag">next {{.NextTicketIDPreview}}</span></h2>
			<form method="post" action="/tickets">
				<input type="hidden" name="filter_assignee" value="{{.FilterAssignee}}" />
				<input type="hidden" name="filter_type" value="{{.FilterType}}" />
				<div class="grid">
					<input type="text" name="title" placeholder="Ticket title" required maxlength="120" />
					<input type="text" name="assignee" placeholder="Assignee" maxlength="60" />
					<select name="type">
						<option value="story">story</option>
						<option value="bug">bug</option>
						<option value="task">task</option>
					</select>
					<input type="number" name="story_points" min="1" max="21" value="3" required />
					<button class="btn btn-main" type="submit">Create Ticket</button>
				</div>
				<textarea name="description" placeholder="Description"></textarea>
			</form>
		</section>

		<section class="board">
			{{range .Columns}}
			<article class="column">
				<h3>{{.Label}}</h3>
				<div class="count">{{len .Tickets}} ticket(s)</div>
				{{if .Tickets}}
					{{range .Tickets}}
					<div class="ticket">
						<div class="tag">{{.ID}} · {{.Type}}</div>
						<h4>{{.Title}}</h4>
						<p>{{.Description}}</p>
						<div class="meta">
							<span>{{.StoryPoints}} SP</span>
							<span>{{if .Assignee}}{{.Assignee}}{{else}}Unassigned{{end}}</span>
						</div>
						<div class="actions">
							<form class="move" method="post" action="/tickets/{{.ID}}/move">
								<input type="hidden" name="filter_assignee" value="{{$.FilterAssignee}}" />
								<input type="hidden" name="filter_type" value="{{$.FilterType}}" />
								{{if .AllowedMoves}}
								<select name="status">
									{{range .AllowedMoves}}
									<option value="{{.Key}}">{{.Label}}</option>
									{{end}}
								</select>
								<button class="btn btn-dark" type="submit">Move</button>
								{{else}}
								<span class="tag">No allowed transitions</span>
								{{end}}
							</form>
							<div class="small-row">
								<a class="link-btn" href="/?assignee={{urlquery $.FilterAssignee}}&type={{urlquery $.FilterType}}&edit={{.ID}}">Edit</a>
								<form method="post" action="/tickets/{{.ID}}/delete" onsubmit="return confirm('Delete this ticket?');">
									<input type="hidden" name="filter_assignee" value="{{$.FilterAssignee}}" />
									<input type="hidden" name="filter_type" value="{{$.FilterType}}" />
									<button class="btn btn-danger" type="submit">Delete</button>
								</form>
							</div>
						</div>

						{{if eq $.EditTicketID .ID}}
						<form class="edit-form" method="post" action="/tickets/{{.ID}}/edit">
							<input type="hidden" name="filter_assignee" value="{{$.FilterAssignee}}" />
							<input type="hidden" name="filter_type" value="{{$.FilterType}}" />
							<input type="text" name="title" value="{{.Title}}" required maxlength="120" />
							<input type="text" name="assignee" value="{{.Assignee}}" maxlength="60" />
							<select name="type">
								<option value="story" {{if eq .Type "story"}}selected{{end}}>story</option>
								<option value="bug" {{if eq .Type "bug"}}selected{{end}}>bug</option>
								<option value="task" {{if eq .Type "task"}}selected{{end}}>task</option>
							</select>
							<input type="number" name="story_points" min="1" max="21" value="{{.StoryPoints}}" required />
							<textarea name="description">{{.Description}}</textarea>
							<div class="small-row">
								<button class="btn btn-main" type="submit">Save</button>
								<a class="link-btn" href="/?assignee={{urlquery $.FilterAssignee}}&type={{urlquery $.FilterType}}">Cancel</a>
							</div>
						</form>
						{{end}}
					</div>
					{{end}}
				{{else}}
				<div class="empty">No tickets here yet.</div>
				{{end}}
			</article>
			{{end}}
		</section>

		<footer class="footer">
			<div>Prometheus metrics: <a href="/metrics">/metrics</a></div>
			<div>Closed Story Points: {{.CompletedPoints}}</div>
		</footer>
	</main>
</body>
</html>`

	return template.Must(template.New("dashboard").Parse(page))
}

func main() {
	ctx := context.Background()

	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		dsn = "postgres://scrum:scrum@localhost:5432/scrum_dashboard?sslmode=disable"
	}

	store, err := newStore(ctx, dsn)
	if err != nil {
		log.Fatalf("failed to init postgres store: %v", err)
	}
	defer store.db.Close()

	tmpl := mustTemplate()
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		filter := parseFilter(r)
		editID := strings.TrimSpace(r.URL.Query().Get("edit"))
		data, err := store.DashboardSnapshot(r.Context(), filter, editID)
		if err != nil {
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			log.Printf("dashboard snapshot error: %v", err)
			return
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/tickets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		title := strings.TrimSpace(r.FormValue("title"))
		description := strings.TrimSpace(r.FormValue("description"))
		assignee := strings.TrimSpace(r.FormValue("assignee"))
		ticketType := normalizeTicketType(r.FormValue("type"))
		storyPoints, err := strconv.Atoi(strings.TrimSpace(r.FormValue("story_points")))
		if err != nil || storyPoints < 1 || storyPoints > 21 {
			http.Error(w, "story_points must be between 1 and 21", http.StatusBadRequest)
			return
		}
		if title == "" {
			http.Error(w, "title is required", http.StatusBadRequest)
			return
		}

		if err := store.CreateTicket(r.Context(), title, description, assignee, ticketType, storyPoints); err != nil {
			http.Error(w, "failed to create ticket", http.StatusInternalServerError)
			log.Printf("create ticket error: %v", err)
			return
		}

		redirectDashboard(w, r, parseReturnFilter(r), "")
	})

	mux.HandleFunc("/tickets/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		id, action, ok := parseTicketAction(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}

		filter := parseReturnFilter(r)

		switch action {
		case "move":
			next := TicketStatus(strings.TrimSpace(r.FormValue("status")))
			if err := store.MoveTicket(r.Context(), id, next); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			redirectDashboard(w, r, filter, "")
			return
		case "edit":
			title := strings.TrimSpace(r.FormValue("title"))
			description := strings.TrimSpace(r.FormValue("description"))
			assignee := strings.TrimSpace(r.FormValue("assignee"))
			ticketType := normalizeTicketType(r.FormValue("type"))
			storyPoints, err := strconv.Atoi(strings.TrimSpace(r.FormValue("story_points")))
			if err != nil || storyPoints < 1 || storyPoints > 21 {
				http.Error(w, "story_points must be between 1 and 21", http.StatusBadRequest)
				return
			}
			if title == "" {
				http.Error(w, "title is required", http.StatusBadRequest)
				return
			}
			if err := store.UpdateTicket(r.Context(), id, title, description, assignee, ticketType, storyPoints); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "failed to update ticket", http.StatusInternalServerError)
				return
			}
			redirectDashboard(w, r, filter, "")
			return
		case "delete":
			if err := store.DeleteTicket(r.Context(), id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "failed to delete ticket", http.StatusInternalServerError)
				return
			}
			redirectDashboard(w, r, filter, "")
			return
		default:
			http.NotFound(w, r)
			return
		}
	})

	mux.Handle("/metrics", promhttp.Handler())

	log.Println("Scrum Dashboard server starting on :8080...")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
