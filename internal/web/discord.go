package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// discordUser は Discord の /users/@me のうち使う項目。
type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

func (u discordUser) displayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

var errNotMember = errors.New("not a member of the guild")

func (s *Server) discordScopes() string {
	if s.cfg.Web.DiscordGuildID != "" {
		// ギルドのメンバーかどうかとロールを確かめるため
		return "identify guilds.members.read"
	}
	return "identify"
}

func (s *Server) redirectURI() string { return s.cfg.PublicURL + "/auth/discord/callback" }

func (s *Server) authorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", s.cfg.Web.DiscordClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", s.redirectURI())
	q.Set("scope", s.discordScopes())
	q.Set("state", state)
	q.Set("prompt", "none")
	return s.cfg.DiscordAuthorizeURL + "?" + q.Encode()
}

// exchangeCode は認可コードを Discord のアクセストークンに交換する（トークンは保存しない）。
func (s *Server) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.redirectURI())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.DiscordAPI+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(s.cfg.Web.DiscordClientID, s.cfg.Web.DiscordClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.discordDo(req, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("discord: empty access token")
	}
	return out.AccessToken, nil
}

func (s *Server) discordGet(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.DiscordAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return s.discordDo(req, out)
}

type discordStatusError struct{ status int }

func (e discordStatusError) Error() string { return fmt.Sprintf("discord: status %d", e.status) }

func (s *Server) discordDo(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return discordStatusError{status: resp.StatusCode}
	}
	return json.Unmarshal(body, out)
}

// checkMembership はギルドのメンバーか、指定のロールを持つかを確かめる。
func (s *Server) checkMembership(ctx context.Context, token string) error {
	var member struct {
		Roles []string `json:"roles"`
	}
	err := s.discordGet(ctx, token, "/users/@me/guilds/"+url.PathEscape(s.cfg.Web.DiscordGuildID)+"/member", &member)
	var statusErr discordStatusError
	if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
		return errNotMember
	}
	if err != nil {
		return err
	}
	if len(s.cfg.Web.DiscordRoleIDs) == 0 {
		return nil
	}
	for _, have := range member.Roles {
		for _, want := range s.cfg.Web.DiscordRoleIDs {
			if have == want {
				return nil
			}
		}
	}
	return errNotMember
}

// authorize は Discord の利用者がログインしてよいかを判断する。
// 管理者と DISCORD_ALLOWED_USER_IDS は常に許可し、それ以外はギルド（とロール）の条件で判断する。
// ギルドの指定がなければ、管理者と個別に許可した利用者だけがログインできる。
func (s *Server) authorize(ctx context.Context, token string, u discordUser) error {
	if s.isAdminID(u.ID) || contains(s.cfg.Web.AllowedUserIDs, u.ID) {
		return nil
	}
	if s.cfg.Web.DiscordGuildID == "" {
		return errNotMember
	}
	return s.checkMembership(ctx, token)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
