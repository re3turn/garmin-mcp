package server

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/re3turn/garmin-personal-mcp/internal/garmin"
)

const defaultSpanDays = 7

var noData = map[string]any{"no_data": garmin.NoDataMessage}

var (
	readsGarmin    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)}
	savesLocalFile = &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(true)}
)

type Server struct {
	client *garmin.Client
	today  func() time.Time
}

func New(client *garmin.Client, version string) *mcp.Server {
	s := &Server{client: client, today: time.Now}
	return s.mcpServer(version)
}

func (s *Server) mcpServer(version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garmin-personal-mcp", Version: version}, nil)
	s.addTools(server)
	return server
}

// addTool wraps every result the same way: structuredContent should be a JSON
// object rather than an array, an empty list should read as "no results"
// rather than as nothing, and Garmin's empty 204 needs an explanation.
func addTool[In any](server *mcp.Server, tool *mcp.Tool, run func(context.Context, In) (any, error)) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		result, err := run(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, shape(result), nil
	})
}

func shape(result any) any {
	switch v := result.(type) {
	case nil:
		return noData
	case []any:
		return map[string]any{"results": v}
	default:
		return v
	}
}

func (s *Server) parseDay(value string) (time.Time, error) {
	if value == "" {
		now := s.today()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	day, err := time.Parse(garmin.DateLayout, value)
	if err != nil {
		// Covers both a wrong format and a date that doesn't exist (2026-02-30).
		return time.Time{}, fmt.Errorf("%q is not a valid date; dates must be real calendar days written as YYYY-MM-DD", value)
	}
	return day, nil
}

// parseRange defaults to the defaultSpanDays days ending on end.
func (s *Server) parseRange(start, end string) (time.Time, time.Time, error) {
	last, err := s.parseDay(end)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if start == "" {
		return last.AddDate(0, 0, -(defaultSpanDays - 1)), last, nil
	}
	first, err := s.parseDay(start)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if first.After(last) && end == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("start (%s) is after today (%s), which end defaults to; pass a start on or before today",
			first.Format(garmin.DateLayout), last.Format(garmin.DateLayout))
	}
	if first.After(last) {
		return time.Time{}, time.Time{}, fmt.Errorf("start (%s) must be on or before end (%s); swap them",
			first.Format(garmin.DateLayout), last.Format(garmin.DateLayout))
	}
	return first, last, nil
}

func valueOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
