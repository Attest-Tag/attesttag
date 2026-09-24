package app

import (
	"context"
	"strings"
)

// The console assistant's own transcript.
//
// Its own table rather than a row in `turns`: that one is keyed on team_id with no org_id, so a
// console account — which has no workspace — would write with an empty key that every other
// organisation could read back. See migrations/sqlite/0006_assistant_turns.sql.
//
// Append-only. Nothing here updates or deletes a row; the retention sweep is the only thing
// that removes one, on the organisation's own data_retention_days.

// AssistantTurn is one question and what came back.
type AssistantTurn struct {
	ID int64  `json:"id"`
	At string `json:"at"`
	// Actor is the console account's public id; ActorName is who to show once that account is
	// gone, written out at the time for the reason the audit log writes its actor out.
	Actor        string  `json:"-"`
	ActorName    string  `json:"actor_name"`
	Conversation string  `json:"conversation"`
	Question     string  `json:"question"`
	Reply        string  `json:"reply"`
	Model        string  `json:"model"`
	Page         string  `json:"page"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	CostUSD      float64 `json:"cost_usd"`
	ToolCalls    int     `json:"tool_calls"`
	Proposals    int     `json:"proposals"`
	Error        string  `json:"error"`
}

const assistantTurnCols = `select id, created_at, coalesce(actor_name,''), coalesce(conversation,''),
	coalesce(question,''), coalesce(reply,''), coalesce(model,''), coalesce(page,''),
	tokens_in, tokens_out, cost_usd, tool_calls, proposals, coalesce(error,'')
	from assistant_turns where org_id=?`

// AddAssistantTurn records one console question and its answer.
func (s *Store) AddAssistantTurn(ctx context.Context, orgID int64, t AssistantTurn) {
	s.db.ExecContext(ctx, `insert into assistant_turns
		(org_id, actor, actor_name, conversation, question, reply, model, page, tokens_in, tokens_out, cost_usd, tool_calls, proposals, error)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID, t.Actor, t.ActorName, t.Conversation, t.Question, t.Reply, t.Model, t.Page,
		t.TokensIn, t.TokensOut, t.CostUSD, t.ToolCalls, t.Proposals, t.Error)
}

// AssistantTurns lists one organisation's console conversations, newest first. `contains`
// narrows to questions and answers mentioning it, which is what makes the page searchable
// without a second index.
func (s *Store) AssistantTurns(ctx context.Context, orgID int64, contains string, limit int) ([]AssistantTurn, error) {
	q, argv := assistantTurnCols, []any{orgID}
	if c := strings.TrimSpace(contains); c != "" {
		q += ` and (question like ? or reply like ?)`
		argv = append(argv, "%"+c+"%", "%"+c+"%")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += ` order by id desc limit ?`
	argv = append(argv, limit)

	rows, err := s.db.QueryContext(ctx, q, argv...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantTurn{}
	for rows.Next() {
		var t AssistantTurn
		if err := rows.Scan(&t.ID, &t.At, &t.ActorName, &t.Conversation, &t.Question, &t.Reply,
			&t.Model, &t.Page, &t.TokensIn, &t.TokensOut, &t.CostUSD, &t.ToolCalls, &t.Proposals, &t.Error); err != nil {
			return out, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
