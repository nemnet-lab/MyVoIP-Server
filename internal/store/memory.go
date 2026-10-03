package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory は試験用の Store 実装。
type Memory struct {
	mu         sync.Mutex
	extensions map[string]Extension
	codes      map[string]EnrollmentCode
	devices    map[string]Device
	tokens     map[string]Token
	calls      map[string]Call
	history    map[string]HistoryItem
	seq        int64
}

func NewMemory() *Memory {
	return &Memory{
		extensions: map[string]Extension{},
		codes:      map[string]EnrollmentCode{},
		devices:    map[string]Device{},
		tokens:     map[string]Token{},
		calls:      map[string]Call{},
		history:    map[string]HistoryItem{},
	}
}

func (m *Memory) UpsertExtension(_ context.Context, e Extension) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.UpdatedAt = time.Now()
	m.extensions[e.Number] = e
	return nil
}

func (m *Memory) GetExtension(_ context.Context, number string) (Extension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok {
		return e, ErrNotFound
	}
	return e, nil
}

func (m *Memory) ListExtensions(_ context.Context) ([]Extension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []Extension
	for _, e := range m.extensions {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	return list, nil
}

func (m *Memory) CreateEnrollmentCode(_ context.Context, c EnrollmentCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.CreatedAt = time.Now()
	m.codes[c.CodeHash] = c
	return nil
}

func (m *Memory) ConsumeEnrollmentCode(_ context.Context, codeHash string, now time.Time) (EnrollmentCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[codeHash]
	switch {
	case !ok:
		return c, ErrNotFound
	case c.UsedAt != nil:
		return c, ErrUsed
	case !c.ExpiresAt.After(now):
		return c, ErrExpired
	}
	c.UsedAt = &now
	m.codes[codeHash] = c
	return c, nil
}

func (m *Memory) CreateDevice(_ context.Context, d Device, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, existing := range m.devices {
		if existing.Extension == d.Extension && existing.RevokedAt == nil {
			existing.RevokedAt = &now
			existing.VoIPToken, existing.AlertToken = "", ""
			m.devices[id] = existing
			m.revokeTokensLocked(id, now)
		}
	}
	d.Enabled = true
	d.CreatedAt = now
	m.devices[d.ID] = d
	return nil
}

func (m *Memory) revokeTokensLocked(deviceID string, now time.Time) {
	for h, t := range m.tokens {
		if t.DeviceID == deviceID && t.RevokedAt == nil {
			t.RevokedAt = &now
			m.tokens[h] = t
		}
	}
}

func (m *Memory) GetDevice(_ context.Context, id string) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}

func (m *Memory) ActiveDeviceForExtension(_ context.Context, extension string) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.Extension == extension && d.RevokedAt == nil {
			return d, nil
		}
	}
	return Device{}, ErrNotFound
}

func (m *Memory) ListDevices(_ context.Context) ([]Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []Device
	for _, d := range m.devices {
		list = append(list, d)
	}
	return list, nil
}

func (m *Memory) update(id string, f func(*Device)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	f(&d)
	m.devices[id] = d
	return nil
}

func (m *Memory) UpdateDevicePush(_ context.Context, id, voipToken, alertToken, environment string) error {
	return m.update(id, func(d *Device) {
		if d.RevokedAt == nil {
			d.VoIPToken, d.AlertToken, d.PushEnvironment = voipToken, alertToken, environment
		}
	})
}

func (m *Memory) ClearVoIPToken(_ context.Context, id, token string) error {
	return m.update(id, func(d *Device) {
		if d.VoIPToken == token {
			d.VoIPToken = ""
		}
	})
}

func (m *Memory) SetDeviceEnabled(_ context.Context, id string, enabled bool) error {
	return m.update(id, func(d *Device) { d.Enabled = enabled })
}

func (m *Memory) RevokeDevice(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	if d.RevokedAt == nil {
		d.RevokedAt = &now
	}
	d.VoIPToken, d.AlertToken = "", ""
	m.devices[id] = d
	m.revokeTokensLocked(id, now)
	return nil
}

func (m *Memory) CreateToken(_ context.Context, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[t.Hash] = t
	return nil
}

func (m *Memory) GetToken(_ context.Context, hash string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[hash]
	if !ok {
		return t, ErrNotFound
	}
	return t, nil
}

func (m *Memory) RevokeToken(_ context.Context, hash string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[hash]; ok && t.RevokedAt == nil {
		t.RevokedAt = &now
		m.tokens[hash] = t
	}
	return nil
}

func (m *Memory) SaveCall(_ context.Context, c Call) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[c.ID] = c
	return nil
}

func (m *Memory) GetCall(_ context.Context, id string) (Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calls[id]
	if !ok {
		return c, ErrNotFound
	}
	return c, nil
}

func (m *Memory) ListUnfinishedCalls(_ context.Context) ([]Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []Call
	for _, c := range m.calls {
		if c.State != "ended" {
			list = append(list, c)
		}
	}
	return list, nil
}

func (m *Memory) AddHistory(_ context.Context, h HistoryItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.history[h.ID]; exists {
		return nil
	}
	m.seq++
	h.Seq = m.seq
	m.history[h.ID] = h
	return nil
}

func (m *Memory) ListHistory(_ context.Context, extension string, cursor int64, limit int) ([]HistoryItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []HistoryItem
	for _, h := range m.history {
		if h.Extension == extension && h.Seq > cursor {
			list = append(list, h)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (m *Memory) DeleteHistory(_ context.Context, extension, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.history[id]
	if !ok || h.Extension != extension || h.DeletedAt != nil {
		return ErrNotFound
	}
	m.seq++
	h.DeletedAt, h.Seq = &now, m.seq
	m.history[id] = h
	return nil
}

func (m *Memory) DeleteAllHistory(_ context.Context, extension string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.history {
		if h.Extension == extension && h.DeletedAt == nil {
			m.seq++
			h.DeletedAt, h.Seq = &now, m.seq
			m.history[id] = h
		}
	}
	return nil
}

func (m *Memory) PurgeHistoryBefore(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, h := range m.history {
		if h.StartedAt.Before(before) {
			delete(m.history, id)
			n++
		}
	}
	return n, nil
}
