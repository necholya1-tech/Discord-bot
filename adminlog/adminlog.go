package adminlog

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type Logger struct {
	s            *discordgo.Session
	guildID      string
	logChannelID string
	mu           sync.RWMutex
	memberCache  map[string]*memberSnapshot // key: userID
}

type memberSnapshot struct {
	Nick  string
	Roles map[string]struct{}
}

// служебная инфа из аудит-лога: кто сделал и по какой причине
type execInfo struct {
	User   *discordgo.User
	Reason string
}

// Init регистрирует хэндлеры и запускает кэш.
func Init(s *discordgo.Session, guildID, logChannelID string) *Logger {
	l := &Logger{
		s:            s,
		guildID:      guildID,
		logChannelID: logChannelID,
		memberCache:  make(map[string]*memberSnapshot),
	}

	// начальная прогрузка членов (по возможности)
	go l.primeCache()

	// события
	s.AddHandler(l.onGuildMemberUpdate) // никнеймы/роли
	s.AddHandler(l.onGuildMemberRemove) // кик
	s.AddHandler(l.onGuildBanAdd)       // бан
	s.AddHandler(l.onGuildBanRemove)    // разбан

	return l
}

// ----------------- Handlers -----------------

func (l *Logger) onGuildMemberUpdate(_ *discordgo.Session, ev *discordgo.GuildMemberUpdate) {
	if ev.GuildID != l.guildID || ev.Member == nil || ev.User == nil {
		return
	}

	before := l.getSnapshot(ev.User.ID)
	after := snapshotFromMember(ev.Member)

	// ✏️ смена никнейма
	if before != nil && before.Nick != after.Nick {
		exec := l.lookupExecutor(ev.GuildID, ev.User.ID, discordgo.AuditLogActionMemberUpdate)
		l.postNickChange(ev.User, safe(before.Nick), safe(after.Nick), exec)
	}

	// 🛠 роли
	added, removed := diffRoles(before, after)
	if len(added) > 0 || len(removed) > 0 {
		exec := l.lookupExecutor(ev.GuildID, ev.User.ID, discordgo.AuditLogActionMemberRoleUpdate)
		l.postRoleUpdate(ev.User, added, removed, exec)
	}

	// обновляем кэш
	l.setSnapshot(ev.User.ID, after)
}

func (l *Logger) onGuildMemberRemove(_ *discordgo.Session, ev *discordgo.GuildMemberRemove) {
	if ev.GuildID != l.guildID || ev.User == nil {
		return
	}
	// 👢 отличаем кик от обычного выхода
	exec := l.lookupExecutor(ev.GuildID, ev.User.ID, discordgo.AuditLogActionMemberKick)
	if exec != nil {
		l.postKick(ev.User, exec)
	}
	l.deleteSnapshot(ev.User.ID)
}

func (l *Logger) onGuildBanAdd(_ *discordgo.Session, ev *discordgo.GuildBanAdd) {
	if ev.GuildID != l.guildID || ev.User == nil {
		return
	}
	exec := l.lookupExecutor(ev.GuildID, ev.User.ID, discordgo.AuditLogActionMemberBanAdd)
	l.postBan(ev.User, exec)
	l.deleteSnapshot(ev.User.ID)
}

func (l *Logger) onGuildBanRemove(_ *discordgo.Session, ev *discordgo.GuildBanRemove) {
	if ev.GuildID != l.guildID || ev.User == nil {
		return
	}
	exec := l.lookupExecutor(ev.GuildID, ev.User.ID, discordgo.AuditLogActionMemberBanRemove)
	l.postUnban(ev.User, exec)
}

// ----------------- Helpers (state/cache) -----------------

func (l *Logger) primeCache() {
	after := ""
	for {
		members, err := l.s.GuildMembers(l.guildID, after, 1000)
		if err != nil || len(members) == 0 {
			return
		}
		for _, m := range members {
			l.setSnapshot(m.User.ID, snapshotFromMember(m))
			after = m.User.ID
		}
		if len(members) < 1000 {
			return
		}
	}
}

func snapshotFromMember(m *discordgo.Member) *memberSnapshot {
	ms := &memberSnapshot{
		Nick:  m.Nick,
		Roles: make(map[string]struct{}, len(m.Roles)),
	}
	for _, r := range m.Roles {
		ms.Roles[r] = struct{}{}
	}
	return ms
}

func diffRoles(before, after *memberSnapshot) (added, removed []string) {
	if after == nil {
		return
	}
	if before == nil {
		for r := range after.Roles {
			added = append(added, r)
		}
		return
	}
	for r := range after.Roles {
		if _, ok := before.Roles[r]; !ok {
			added = append(added, r)
		}
	}
	for r := range before.Roles {
		if _, ok := after.Roles[r]; !ok {
			removed = append(removed, r)
		}
	}
	return
}

func (l *Logger) getSnapshot(userID string) *memberSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.memberCache[userID]
}

func (l *Logger) setSnapshot(userID string, snap *memberSnapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.memberCache[userID] = snap
}

func (l *Logger) deleteSnapshot(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.memberCache, userID)
}

// ----------------- Audit Log lookup -----------------

// В твоей версии discordgo:
// - action — discordgo.AuditLogAction (enum).
// - GuildAuditLog требует int(action).
// - e.ActionType — *discordgo.AuditLogAction (указатель).
func (l *Logger) lookupExecutor(guildID, targetUserID string, action discordgo.AuditLogAction) *execInfo {
	al, err := l.s.GuildAuditLog(guildID, "", "", int(action), 50)
	if err != nil || al == nil {
		return nil
	}
	for _, e := range al.AuditLogEntries {
		if e.ActionType == nil || *e.ActionType != action {
			continue
		}
		if e.TargetID != targetUserID {
			continue
		}
		info := &execInfo{
			User:   nil,
			Reason: strings.TrimSpace(e.Reason),
		}
		if e.UserID != "" {
			info.User = &discordgo.User{ID: e.UserID}
		}
		return info
	}
	return nil
}

// ----------------- Posting (embeds) -----------------

func (l *Logger) postNickChange(target *discordgo.User, oldNick, newNick string, exec *execInfo) {
	embed := &discordgo.MessageEmbed{
		Title:     "✏️ Никнейм обновлён",
		Color:     0x3498DB,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Пользователь", Value: userTag(target), Inline: true},
			{Name: "Модератор", Value: formatExec(exec), Inline: true},
			{Name: "Изменение", Value: fmt.Sprintf("%s → %s", codeOrDash(oldNick), codeOrDash(newNick))},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

func (l *Logger) postRoleUpdate(target *discordgo.User, addedIDs, removedIDs []string, exec *execInfo) {
	added := ResolveRoleNames(l.s, l.guildID, addedIDs)
	removed := ResolveRoleNames(l.s, l.guildID, removedIDs)

	embed := &discordgo.MessageEmbed{
		Title:     "🛠 Роли обновлены",
		Color:     0xFFA500,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Пользователь", Value: userTag(target), Inline: true},
			{Name: "Модератор", Value: formatExec(exec), Inline: true},
			{Name: "Добавлены", Value: bullet(added)},
			{Name: "Убраны", Value: bullet(removed)},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

func (l *Logger) postKick(target *discordgo.User, exec *execInfo) {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: userTag(target), Inline: true},
		{Name: "Модератор", Value: formatExec(exec), Inline: true},
	}
	if exec != nil && exec.Reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(exec.Reason)})
	}

	embed := &discordgo.MessageEmbed{
		Title:     "👢 Кик",
		Color:     0xE67E22,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

func (l *Logger) postBan(target *discordgo.User, exec *execInfo) {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: userTag(target), Inline: true},
		{Name: "Модератор", Value: formatExec(exec), Inline: true},
	}
	if exec != nil && exec.Reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(exec.Reason)})
	}

	embed := &discordgo.MessageEmbed{
		Title:     "⛔ Бан",
		Color:     0xE74C3C,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

func (l *Logger) postUnban(target *discordgo.User, exec *execInfo) {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: userTag(target), Inline: true},
		{Name: "Модератор", Value: formatExec(exec), Inline: true},
	}
	if exec != nil && exec.Reason != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(exec.Reason)})
	}

	embed := &discordgo.MessageEmbed{
		Title:     "♻️ Разбан",
		Color:     0x2ECC71,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

func (l *Logger) sendEmbed(embed *discordgo.MessageEmbed) {
	if l.logChannelID == "" {
		log.Println("[adminlog]", embed.Title)
		return
	}
	_, _ = l.s.ChannelMessageSendEmbed(l.logChannelID, embed)
}

// ----------------- Small utils -----------------

func userTag(u *discordgo.User) string {
	if u == nil {
		return "—"
	}
	return fmt.Sprintf("<@%s> (%s)", u.ID, u.Username)
}

func formatExec(exec *execInfo) string {
	if exec == nil || exec.User == nil || exec.User.ID == "" {
		return "—"
	}
	return fmt.Sprintf("<@%s>", exec.User.ID)
}

func safe(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func codeOrDash(s string) string {
	s = safe(s)
	if s == "—" {
		return s
	}
	return fmt.Sprintf("`%s`", s)
}

func code(s string) string {
	return fmt.Sprintf("`%s`", s)
}

func avatarURL(u *discordgo.User) string {
	// Простейшая генерация URL (без зависимостей от версии discordgo)
	if u == nil {
		return ""
	}
	if u.Avatar != "" {
		return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=256", u.ID, u.Avatar)
	}
	// дефолтные аватарки 0-5 — возьмём 0
	return "https://cdn.discordapp.com/embed/avatars/0.png"
}

// Удобный резолвер имён ролей (опционально).
func ResolveRoleNames(s *discordgo.Session, guildID string, ids []string) []string {
	if len(ids) == 0 {
		return []string{"—"}
	}
	g, err := s.State.Guild(guildID)
	if err != nil || g == nil {
		return ids
	}
	nameByID := make(map[string]string, len(g.Roles))
	for _, r := range g.Roles {
		nameByID[r.ID] = "@" + r.Name
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := nameByID[id]; ok {
			out = append(out, n)
		} else {
			out = append(out, id)
		}
	}
	return out
}

func bullet(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}

// PostMute — лог о выдаче мута (минуты > 0 — длительность; 0/отрицательное — "не задано").
func (l *Logger) PostMute(target, moderator *discordgo.User, reason string, minutes int) {
	extra := "не задано"
	if minutes > 0 {
		extra = fmt.Sprintf("%d мин.", minutes)
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: userTag(target), Inline: true},
		{Name: "Модератор", Value: formatExec(&execInfo{User: moderator}), Inline: true},
		{Name: "Длительность", Value: code(extra), Inline: true},
	}
	if strings.TrimSpace(reason) != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(reason)})
	}
	embed := &discordgo.MessageEmbed{
		Title:     "⛔ Мут",
		Color:     0xE74C3C,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

// PostUnmute — лог о снятии мута.
func (l *Logger) PostUnmute(target, moderator *discordgo.User, reason string) {
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: userTag(target), Inline: true},
		{Name: "Модератор", Value: formatExec(&execInfo{User: moderator}), Inline: true},
	}
	if strings.TrimSpace(reason) != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(reason)})
	}
	embed := &discordgo.MessageEmbed{
		Title:     "♻️ Размут",
		Color:     0x2ECC71,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04")),
		},
	}
	l.sendEmbed(embed)
}

