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

	users     map[string]User
	sessions  map[string]WebSession
	passcodes map[string]SupportPasscode
	audit     []AuditEntry
}

func NewMemory() *Memory {
	return &Memory{
		extensions: map[string]Extension{},
		codes:      map[string]EnrollmentCode{},
		devices:    map[string]Device{},
		tokens:     map[string]Token{},
		calls:      map[string]Call{},
		history:    map[string]HistoryItem{},
		users:      map[string]User{},
		sessions:   map[string]WebSession{},
		passcodes:  map[string]SupportPasscode{},
	}
}

func (m *Memory) UpsertExtension(_ context.Context, e Extension) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.UpdatedAt = time.Now()
	if old, ok := m.extensions[e.Number]; ok {
		if e.MobilePasswordEnc == nil {
			e.MobilePasswordEnc = old.MobilePasswordEnc
		}
		// 割り当ては UpsertExtension では変えない（PostgreSQL 実装と同じ）
		e.OwnerUserID, e.PBXExtension, e.Label, e.AssignedAt = old.OwnerUserID, old.PBXExtension, old.Label, old.AssignedAt
	} else {
		e.OwnerUserID, e.PBXExtension, e.Label, e.AssignedAt = "", "", "", nil
	}
	m.extensions[e.Number] = e
	return nil
}

func (m *Memory) SetMobilePassword(_ context.Context, number string, enc []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok {
		return ErrNotFound
	}
	e.MobilePasswordEnc = enc
	m.extensions[number] = e
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
	sortExtensions(list)
	return list, nil
}

func sortExtensions(list []Extension) {
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].Number) != len(list[j].Number) {
			return len(list[i].Number) < len(list[j].Number)
		}
		return list[i].Number < list[j].Number
	})
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

// --- Web 管理画面

func (m *Memory) UpsertDiscordUser(_ context.Context, u User, now time.Time) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, existing := range m.users {
		if existing.DiscordID == u.DiscordID {
			existing.DiscordUsername, existing.DisplayName, existing.LastLoginAt = u.DiscordUsername, u.DisplayName, &now
			m.users[id] = existing
			return existing, nil
		}
	}
	u.CreatedAt, u.LastLoginAt, u.Disabled = now, &now, false
	m.users[u.ID] = u
	return u, nil
}

func (m *Memory) GetUser(_ context.Context, id string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return u, ErrNotFound
	}
	return u, nil
}

func (m *Memory) ListUsers(_ context.Context) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []User
	for _, u := range m.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list, nil
}

func (m *Memory) SetUserDisabled(_ context.Context, id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.Disabled = disabled
	m.users[id] = u
	return nil
}

func (m *Memory) CreateWebSession(_ context.Context, s WebSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.CreatedAt = time.Now()
	m.sessions[s.Hash] = s
	return nil
}

func (m *Memory) GetWebSession(_ context.Context, hash string) (WebSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[hash]
	if !ok {
		return s, ErrNotFound
	}
	return s, nil
}

func (m *Memory) SetWebSessionSupport(_ context.Context, hash, supportUserID string, until *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[hash]; ok {
		s.SupportUserID, s.SupportExpiresAt = supportUserID, until
		m.sessions[hash] = s
	}
	return nil
}

func (m *Memory) DeleteWebSession(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, hash)
	return nil
}

func (m *Memory) DeleteUserWebSessions(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, h)
		}
	}
	return nil
}

func (m *Memory) SetUserPBX(_ context.Context, id, host string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.PBXHost, u.PBXPort = host, port
	m.users[id] = u
	return nil
}

func (m *Memory) CreateOwnedExtension(_ context.Context, e Extension, max int, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[e.OwnerUserID]; !ok {
		return ErrNotFound
	}
	owned := 0
	for _, x := range m.extensions {
		if x.OwnerUserID != e.OwnerUserID {
			continue
		}
		if x.SIPUsername == e.SIPUsername {
			return ErrDuplicate
		}
		owned++
	}
	if owned >= max {
		return ErrLimit
	}
	if _, exists := m.extensions[e.Number]; exists {
		return ErrDuplicate
	}
	e.AssignedAt, e.UpdatedAt = &now, now
	m.extensions[e.Number] = e
	return nil
}

func (m *Memory) UpdateExtensionSIP(_ context.Context, number, pbxExtension, sipUser, authUser string, passwordEnc []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok {
		return ErrNotFound
	}
	e.PBXExtension, e.SIPUsername, e.SIPAuthUsername, e.Endpoint = pbxExtension, sipUser, authUser, sipUser
	if passwordEnc != nil {
		e.SIPPasswordEnc = passwordEnc
	}
	m.extensions[number] = e
	return nil
}

func (m *Memory) DeleteExtension(_ context.Context, number string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.extensions[number]; !ok {
		return ErrNotFound
	}
	delete(m.extensions, number)
	for id, d := range m.devices {
		if d.Extension == number {
			delete(m.devices, id)
			for h, t := range m.tokens {
				if t.DeviceID == id {
					delete(m.tokens, h)
				}
			}
		}
	}
	for h, c := range m.codes {
		if c.Extension == number {
			delete(m.codes, h)
		}
	}
	for id, h := range m.history {
		if h.Extension == number {
			delete(m.history, id)
		}
	}
	return nil
}

func (m *Memory) AssignExtension(_ context.Context, number, userID, label string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok || e.OwnerUserID != "" {
		return ErrNotFound
	}
	e.OwnerUserID, e.Label, e.AssignedAt = userID, label, &now
	m.extensions[number] = e
	return nil
}

func (m *Memory) ReleaseExtension(_ context.Context, number string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok {
		return ErrNotFound
	}
	e.OwnerUserID, e.Label, e.AssignedAt = "", "", nil
	m.extensions[number] = e
	for id, d := range m.devices {
		if d.Extension == number && d.RevokedAt == nil {
			d.RevokedAt = &now
			d.VoIPToken, d.AlertToken = "", ""
			m.devices[id] = d
			m.revokeTokensLocked(id, now)
		}
	}
	for h, c := range m.codes {
		if c.Extension == number {
			delete(m.codes, h)
		}
	}
	for id, h := range m.history {
		if h.Extension == number {
			delete(m.history, id)
		}
	}
	return nil
}

func (m *Memory) SetExtensionLabel(_ context.Context, number, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[number]
	if !ok {
		return ErrNotFound
	}
	e.Label = label
	m.extensions[number] = e
	return nil
}

func (m *Memory) ListExtensionsByOwner(_ context.Context, userID string) ([]Extension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []Extension
	for _, e := range m.extensions {
		if e.OwnerUserID == userID {
			list = append(list, e)
		}
	}
	sortExtensions(list)
	return list, nil
}

func (m *Memory) SetSupportPasscode(_ context.Context, c SupportPasscode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.CreatedAt = time.Now()
	m.passcodes[c.UserID] = c
	return nil
}

func (m *Memory) GetSupportPasscode(_ context.Context, userID string) (SupportPasscode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.passcodes[userID]
	if !ok {
		return c, ErrNotFound
	}
	return c, nil
}

func (m *Memory) DeleteSupportPasscode(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.passcodes, userID)
	return nil
}

func (m *Memory) ConsumeSupportPasscode(_ context.Context, codeHash string, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for userID, c := range m.passcodes {
		if c.CodeHash == codeHash && c.ExpiresAt.After(now) {
			delete(m.passcodes, userID)
			return userID, nil
		}
	}
	return "", ErrNotFound
}

func (m *Memory) AddAudit(_ context.Context, a AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.ID = int64(len(m.audit) + 1)
	m.audit = append(m.audit, a)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, userID string, limit int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []AuditEntry
	for i := len(m.audit) - 1; i >= 0 && len(list) < limit; i-- {
		if m.audit[i].UserID == userID {
			list = append(list, m.audit[i])
		}
	}
	return list, nil
}
