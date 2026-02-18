package flow

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/openclaw/mroute/internal/transport"
	"github.com/openclaw/mroute/pkg/types"
)

// Manager handles flow lifecycle and persistence
type Manager struct {
	db      *sql.DB
	engine  *transport.Engine
	onEvent func(flowID, eventType, message, severity string)
	eventMu sync.RWMutex // protects onEvent callback
}

func NewManager(dsn string, engine *transport.Engine) (*Manager, error) {
	var dbPath string
	if len(dsn) > 7 && dsn[:7] == "sqlite:" {
		dbPath = dsn[7:]
	} else {
		dbPath = dsn
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Limit connection pool for SQLite (single-writer)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	m := &Manager{db: db, engine: engine}
	if err := m.initSchema(); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}

	// Wire engine events to flow manager
	engine.SetEventCallback(m.handleEngineEvent)

	log.Println("Flow manager initialized")
	return m, nil
}

func (m *Manager) Close() error {
	return m.db.Close()
}

func (m *Manager) SetEventCallback(cb func(flowID, eventType, message, severity string)) {
	m.eventMu.Lock()
	m.onEvent = cb
	m.eventMu.Unlock()
}

func (m *Manager) initSchema() error {
	_, err := m.db.Exec(`
		CREATE TABLE IF NOT EXISTS flows (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT DEFAULT '',
			status TEXT DEFAULT 'STANDBY',
			data TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			started_at TEXT,
			stopped_at TEXT
		);
		CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY,
			flow_id TEXT NOT NULL,
			type TEXT NOT NULL,
			message TEXT,
			severity TEXT DEFAULT 'info',
			data TEXT DEFAULT '{}',
			timestamp TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_events_flow ON events(flow_id);
		CREATE INDEX IF NOT EXISTS idx_events_ts ON events(timestamp DESC);
	`)
	return err
}

// generateID creates a unique ID with a prefix using crypto/rand
func generateID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// CreateFlow persists a new flow
func (m *Manager) CreateFlow(flow *types.Flow) error {
	if flow.Name == "" {
		return fmt.Errorf("flow name is required")
	}
	if flow.Source == nil && len(flow.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}

	// Validate all sources
	for _, src := range m.allSources(flow) {
		if err := types.ValidateSource(src); err != nil {
			return err
		}
	}
	// Validate all outputs
	for _, out := range flow.Outputs {
		if err := types.ValidateOutput(out); err != nil {
			return err
		}
	}

	flow.Status = types.FlowStandby
	flow.CreatedAt = time.Now()
	flow.UpdatedAt = time.Now()

	if flow.ID == "" {
		flow.ID = generateID("flow_")
	}

	// Set source defaults
	for _, src := range m.allSources(flow) {
		src.Status = types.SourceDisconnected
	}
	for _, out := range flow.Outputs {
		if out.Status == "" {
			out.Status = types.OutputEnabled
		}
	}

	return m.saveFlow(flow)
}

// GetFlow retrieves a flow by ID
func (m *Manager) GetFlow(id string) (*types.Flow, error) {
	var data, createdAt, updatedAt string
	var startedAt, stoppedAt sql.NullString

	err := m.db.QueryRow("SELECT data, created_at, updated_at, started_at, stopped_at FROM flows WHERE id = ?", id).
		Scan(&data, &createdAt, &updatedAt, &startedAt, &stoppedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("flow not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("database error fetching flow %s: %w", id, err)
	}

	var flow types.Flow
	if err := json.Unmarshal([]byte(data), &flow); err != nil {
		return nil, fmt.Errorf("corrupt flow data for %s: %w", id, err)
	}
	flow.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	flow.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if startedAt.Valid {
		t, _ := time.Parse(time.RFC3339, startedAt.String)
		flow.StartedAt = &t
	}
	if stoppedAt.Valid {
		t, _ := time.Parse(time.RFC3339, stoppedAt.String)
		flow.StoppedAt = &t
	}

	// Overlay live status if running
	if m.engine.IsRunning(id) {
		flow.Status = types.FlowActive
	}

	return &flow, nil
}

// ListFlows lists all flows
func (m *Manager) ListFlows() ([]*types.Flow, error) {
	rows, err := m.db.Query("SELECT data, created_at, updated_at, started_at, stopped_at FROM flows ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var flows []*types.Flow
	for rows.Next() {
		var data, createdAt, updatedAt string
		var startedAt, stoppedAt sql.NullString
		if err := rows.Scan(&data, &createdAt, &updatedAt, &startedAt, &stoppedAt); err != nil {
			return nil, err
		}
		var flow types.Flow
		if err := json.Unmarshal([]byte(data), &flow); err != nil {
			log.Printf("skipping corrupt flow: %v", err)
			continue
		}
		flow.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		flow.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		if startedAt.Valid {
			t, _ := time.Parse(time.RFC3339, startedAt.String)
			flow.StartedAt = &t
		}
		if stoppedAt.Valid {
			t, _ := time.Parse(time.RFC3339, stoppedAt.String)
			flow.StoppedAt = &t
		}
		if m.engine.IsRunning(flow.ID) {
			flow.Status = types.FlowActive
		}
		flows = append(flows, &flow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if flows == nil {
		flows = []*types.Flow{}
	}
	return flows, nil
}

// FlowCounts returns total and active flow counts efficiently
func (m *Manager) FlowCounts() (total int, active int, err error) {
	err = m.db.QueryRow("SELECT COUNT(*) FROM flows").Scan(&total)
	if err != nil {
		return
	}
	running := m.engine.ListRunning()
	active = len(running)
	return
}

// DeleteFlow removes a flow (stops it first if running)
func (m *Manager) DeleteFlow(id string) error {
	// Always try to stop - avoids TOCTOU race where flow starts between check and delete
	_ = m.engine.StopFlow(id)
	// Delete flow and its events atomically
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after successful commit

	if _, err := tx.Exec("DELETE FROM events WHERE flow_id = ?", id); err != nil {
		return fmt.Errorf("delete events: %w", err)
	}
	res, err := tx.Exec("DELETE FROM flows WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("flow not found: %s", id)
	}
	return tx.Commit()
}

// StartFlow activates live transport
func (m *Manager) StartFlow(id string) error {
	flow, err := m.GetFlow(id)
	if err != nil {
		return err
	}
	if flow.Status == types.FlowActive {
		return fmt.Errorf("flow %s is already active", id)
	}

	now := time.Now()
	flow.Status = types.FlowStarting
	flow.StartedAt = &now
	flow.StoppedAt = nil
	flow.UpdatedAt = now
	if err := m.saveFlow(flow); err != nil {
		return fmt.Errorf("save flow starting state: %w", err)
	}

	if err := m.engine.StartFlow(flow); err != nil {
		flow.Status = types.FlowError
		m.saveFlow(flow)
		return err
	}

	flow.Status = types.FlowActive
	if err := m.saveFlow(flow); err != nil {
		log.Printf("warning: failed to save active state for flow %s: %v", id, err)
	}

	m.recordEvent(id, "flow_started", fmt.Sprintf("Flow %s started", flow.Name), "info")
	return nil
}

// StopFlow deactivates live transport
func (m *Manager) StopFlow(id string) error {
	flow, err := m.GetFlow(id)
	if err != nil {
		return err
	}

	// Don't stop if already stopped
	if flow.Status == types.FlowStandby && !m.engine.IsRunning(id) {
		return nil
	}

	if err := m.engine.StopFlow(id); err != nil {
		// might already be stopped, that's ok
		log.Printf("engine stop: %v", err)
	}

	now := time.Now()
	flow.Status = types.FlowStandby
	flow.StoppedAt = &now
	flow.UpdatedAt = now
	if err := m.saveFlow(flow); err != nil {
		log.Printf("warning: failed to save stopped state for flow %s: %v", id, err)
	}

	m.recordEvent(id, "flow_stopped", fmt.Sprintf("Flow %s stopped", flow.Name), "info")
	return nil
}

// AddSource adds a source to a flow (for failover)
func (m *Manager) AddSource(flowID string, source *types.Source) error {
	flow, err := m.GetFlow(flowID)
	if err != nil {
		return err
	}

	if err := types.ValidateSource(source); err != nil {
		return err
	}

	// Check for duplicate name
	for _, s := range m.allSources(flow) {
		if s.Name == source.Name {
			return fmt.Errorf("source %s already exists", source.Name)
		}
	}

	// Max 2 sources for failover
	if len(m.allSources(flow)) >= 2 {
		return fmt.Errorf("maximum 2 sources per flow (for failover)")
	}

	source.Status = types.SourceDisconnected
	flow.Sources = append(flow.Sources, source)
	flow.UpdatedAt = time.Now()
	return m.saveFlow(flow)
}

// RemoveSource removes a source from a flow
func (m *Manager) RemoveSource(flowID, sourceName string) error {
	flow, err := m.GetFlow(flowID)
	if err != nil {
		return err
	}

	// Can't remove the only source
	if len(m.allSources(flow)) <= 1 {
		return fmt.Errorf("cannot remove the only source")
	}

	if flow.Source != nil && flow.Source.Name == sourceName {
		// Move first backup to primary
		if len(flow.Sources) > 0 {
			flow.Source = flow.Sources[0]
			flow.Sources = flow.Sources[1:]
		} else {
			return fmt.Errorf("cannot remove the only source")
		}
	} else {
		newSources := make([]*types.Source, 0)
		found := false
		for _, s := range flow.Sources {
			if s.Name == sourceName {
				found = true
			} else {
				newSources = append(newSources, s)
			}
		}
		if !found {
			return fmt.Errorf("source %s not found", sourceName)
		}
		flow.Sources = newSources
	}

	flow.UpdatedAt = time.Now()
	return m.saveFlow(flow)
}

// AddOutput adds an output to a flow
func (m *Manager) AddOutput(flowID string, output *types.Output) error {
	flow, err := m.GetFlow(flowID)
	if err != nil {
		return err
	}
	if err := types.ValidateOutput(output); err != nil {
		return err
	}
	for _, o := range flow.Outputs {
		if o.Name == output.Name {
			return fmt.Errorf("output %s already exists", output.Name)
		}
	}
	if len(flow.Outputs) >= 50 {
		return fmt.Errorf("maximum 50 outputs per flow")
	}
	if output.Status == "" {
		output.Status = types.OutputEnabled
	}
	flow.Outputs = append(flow.Outputs, output)
	flow.UpdatedAt = time.Now()
	return m.saveFlow(flow)
}

// RemoveOutput removes an output from a flow
func (m *Manager) RemoveOutput(flowID, outputName string) error {
	flow, err := m.GetFlow(flowID)
	if err != nil {
		return err
	}
	newOutputs := make([]*types.Output, 0)
	found := false
	for _, o := range flow.Outputs {
		if o.Name == outputName {
			found = true
		} else {
			newOutputs = append(newOutputs, o)
		}
	}
	if !found {
		return fmt.Errorf("output %s not found", outputName)
	}
	flow.Outputs = newOutputs
	flow.UpdatedAt = time.Now()
	return m.saveFlow(flow)
}

// UpdateOutput updates an existing output
func (m *Manager) UpdateOutput(flowID, outputName string, updates *types.Output) error {
	flow, err := m.GetFlow(flowID)
	if err != nil {
		return err
	}
	for _, o := range flow.Outputs {
		if o.Name == outputName {
			if updates.Destination != "" {
				o.Destination = updates.Destination
			}
			if updates.Port != 0 {
				o.Port = updates.Port
			}
			if updates.Protocol != "" {
				o.Protocol = updates.Protocol
			}
			if updates.Status != "" {
				o.Status = updates.Status
			}
			if updates.Description != "" {
				o.Description = updates.Description
			}
			if updates.StreamKey != "" {
				o.StreamKey = updates.StreamKey
			}
			// Validate the composite output after all partial updates
			if err := types.ValidateOutput(o); err != nil {
				return err
			}
			flow.UpdatedAt = time.Now()
			return m.saveFlow(flow)
		}
	}
	return fmt.Errorf("output %s not found", outputName)
}

// GetMetrics returns live metrics for a running flow
func (m *Manager) GetMetrics(flowID string) *types.FlowMetrics {
	return m.engine.GetMetrics(flowID)
}

// GetEvents returns recent events for a flow
func (m *Manager) GetEvents(flowID string, limit int) ([]*types.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if flowID != "" {
		rows, err = m.db.Query("SELECT id, flow_id, type, message, severity, timestamp FROM events WHERE flow_id = ? ORDER BY timestamp DESC LIMIT ?", flowID, limit)
	} else {
		rows, err = m.db.Query("SELECT id, flow_id, type, message, severity, timestamp FROM events ORDER BY timestamp DESC LIMIT ?", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*types.Event
	for rows.Next() {
		var e types.Event
		var ts string
		if err := rows.Scan(&e.ID, &e.FlowID, &e.Type, &e.Message, &e.Severity, &ts); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		events = append(events, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if events == nil {
		events = []*types.Event{}
	}
	return events, nil
}

// Internal helpers

func (m *Manager) saveFlow(flow *types.Flow) error {
	data, err := json.Marshal(flow)
	if err != nil {
		return fmt.Errorf("marshal flow: %w", err)
	}
	var startedAt, stoppedAt sql.NullString
	if flow.StartedAt != nil {
		startedAt = sql.NullString{String: flow.StartedAt.Format(time.RFC3339), Valid: true}
	}
	if flow.StoppedAt != nil {
		stoppedAt = sql.NullString{String: flow.StoppedAt.Format(time.RFC3339), Valid: true}
	}
	_, err = m.db.Exec(`
		INSERT OR REPLACE INTO flows (id, name, description, status, data, created_at, updated_at, started_at, stopped_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, flow.ID, flow.Name, flow.Description, flow.Status, string(data),
		flow.CreatedAt.Format(time.RFC3339), flow.UpdatedAt.Format(time.RFC3339),
		startedAt, stoppedAt)
	return err
}

func (m *Manager) recordEvent(flowID, eventType, message, severity string) {
	id := generateID("evt_")
	if _, err := m.db.Exec("INSERT INTO events (id, flow_id, type, message, severity, timestamp) VALUES (?, ?, ?, ?, ?, ?)",
		id, flowID, eventType, message, severity, time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("record event error: %v", err)
	}
	// Prune old events per flow (keep last 1000)
	m.db.Exec("DELETE FROM events WHERE flow_id = ? AND id NOT IN (SELECT id FROM events WHERE flow_id = ? ORDER BY timestamp DESC LIMIT 1000)", flowID, flowID)
}

func (m *Manager) handleEngineEvent(flowID, eventType, message, severity string) {
	m.recordEvent(flowID, eventType, message, severity)
	m.eventMu.RLock()
	cb := m.onEvent
	m.eventMu.RUnlock()
	if cb != nil {
		cb(flowID, eventType, message, severity)
	}
}

func (m *Manager) allSources(flow *types.Flow) []*types.Source {
	var sources []*types.Source
	if flow.Source != nil {
		sources = append(sources, flow.Source)
	}
	sources = append(sources, flow.Sources...)
	return sources
}
