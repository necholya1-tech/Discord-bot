package mute

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"gosha_bot/adminlog"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry struct {
    GuildID      string
    LogChannelID string
    KeepCategory string
    MutedRoleID  string
    s            *discordgo.Session
    unmuteTimers map[string]*time.Timer
    DB           *pgxpool.Pool

    // ↓ добавь это
    RetentionDays int // через сколько дней чистим completed/canceled; по умолчанию 30

    AdminLog *adminlog.Logger
}


func Register(s *discordgo.Session, guildID, keepCategoryID, logChannelID, mutedRoleID string, db *pgxpool.Pool) (*Registry, error) {
    if mutedRoleID == "" {
        return nil, fmt.Errorf("MUTE_ROLE_ID is empty (нужен ID существующей роли)")
    }
    r := &Registry{
        GuildID:      guildID,
        LogChannelID: logChannelID,
        KeepCategory: keepCategoryID,
        MutedRoleID:  mutedRoleID,
        s:            s,
        unmuteTimers: make(map[string]*time.Timer),
        DB:           db,

        RetentionDays: 30, // ← дефолт
    }
    s.AddHandler(r.onInteraction)

    // запускаем обслуживание, если есть БД
    if r.DB != nil {
        r.startDailyMaintenance()
    }

    return r, nil
}


func (r *Registry) onInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic.GuildID == "" || ic.Member == nil { return }
	if ic.Type != discordgo.InteractionApplicationCommand { return }
	switch ic.ApplicationCommandData().Name {
	case "mute":
		r.handleMute(ic)
	case "unmute":
		r.handleUnmute(ic)
	}
}

// ---------- public handlers ----------

func (r *Registry) handleMute(ic *discordgo.InteractionCreate) {
	// защита от паники + гарантированный ответ
	defer func() {
		if rec := recover(); rec != nil {
			log.Println("[mute] panic:", rec)
			editReply(r.s, ic, "❌ Внутренняя ошибка при обработке команды.")
		}
	}()
	ackEphemeral(r.s, ic) // моментальный ACK

    opts := ic.ApplicationCommandData().Options
if len(opts) < 2 || opts[0].Type != discordgo.ApplicationCommandOptionUser {
    editReply(r.s, ic, "⛔ Неверные аргументы команды.")
    return
}


	gid := ic.GuildID
	target := ic.ApplicationCommandData().Options[0].UserValue(nil)
	minutes := int(ic.ApplicationCommandData().Options[1].IntValue())
	reason := ""
	if len(ic.ApplicationCommandData().Options) >= 3 {
		reason = strings.TrimSpace(ic.ApplicationCommandData().Options[2].StringValue())
	}

	if err := r.checkPermissions(ic, target.ID); err != nil { editReply(r.s, ic, "⛔ "+err.Error()); return }
	if minutes <= 0 { editReply(r.s, ic, "⛔ minutes должен быть > 0"); return }
	if r.DB == nil { editReply(r.s, ic, "⛔ DB недоступна — POSTGRES_DSN не настроен"); return }

	exists, err := r.hasActiveMute(gid, target.ID)
	if err != nil { editReply(r.s, ic, "DB error: "+err.Error()); return }
	if exists { editReply(r.s, ic, "⛔ У пользователя уже есть активный мут."); return }

	rolesToRemove, err := r.computeRolesToRemove(gid, target.ID)
	if err != nil { editReply(r.s, ic, "❌ Роли: "+err.Error()); return }

	removed, err := r.dropRoles(gid, target.ID, rolesToRemove)
	if err != nil { editReply(r.s, ic, "❌ Снятие ролей: "+err.Error()); return }

	if err := r.addMutedRoleOnly(gid, target.ID); err != nil {
		_ = r.restoreRoles(gid, target.ID, removed)
		editReply(r.s, ic, "❌ Не смог выдать мут: "+err.Error()); return
	}

	if err := r.insertMuteRow(gid, target.ID, ic.Member.User.ID, reason, minutes, removed); err != nil {
		_ = r.removeMutedRole(gid, target.ID)
		_ = r.restoreRoles(gid, target.ID, removed)
		editReply(r.s, ic, "❌ DB insert: "+err.Error()); return
	}

	if old, ok := r.unmuteTimers[target.ID]; ok { old.Stop() }
	r.unmuteTimers[target.ID] = time.AfterFunc(time.Duration(minutes)*time.Minute, func() {
		if err := r.forceUnmute(gid, target.ID, "auto-unmute"); err != nil {
			log.Println("[mute] auto-unmute:", err)
		}
	})

	editReply(r.s, ic, fmt.Sprintf("✅ Мут выдан %s на %d мин.", mentionUser(target.ID), minutes))
	if r.AdminLog != nil { r.AdminLog.PostMute(target, ic.Member.User, reason, minutes) } else {
		r.logEmbedMute("⛔ Мут", target, ic.Member.User, reason, minutes, 0xE74C3C)
	}
}


func (r *Registry) handleUnmute(ic *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Println("[unmute] panic:", rec)
			editReply(r.s, ic, "❌ Внутренняя ошибка при обработке команды.")
		}
	}()
	ackEphemeral(r.s, ic)

    opts := ic.ApplicationCommandData().Options
if len(opts) < 2 || opts[0].Type != discordgo.ApplicationCommandOptionUser {
    editReply(r.s, ic, "⛔ Неверные аргументы команды.")
    return
}


	gid := ic.GuildID
	target := ic.ApplicationCommandData().Options[0].UserValue(nil)
	reason := ""
	if len(ic.ApplicationCommandData().Options) >= 2 {
		reason = strings.TrimSpace(ic.ApplicationCommandData().Options[1].StringValue())
	}

	if err := r.checkPermissions(ic, target.ID); err != nil { editReply(r.s, ic, "⛔ "+err.Error()); return }

	if err := r.forceUnmute(gid, target.ID, reason); err != nil {
		editReply(r.s, ic, "❌ Размут не удался: "+err.Error()); return
	}
	if t, ok := r.unmuteTimers[target.ID]; ok { t.Stop(); delete(r.unmuteTimers, target.ID) }

	editReply(r.s, ic, fmt.Sprintf("✅ Мут снят с %s.", mentionUser(target.ID)))
	if r.AdminLog != nil { r.AdminLog.PostUnmute(target, ic.Member.User, reason) } else {
		r.logEmbed("♻️ Размут", target, ic.Member.User, reason, 0x2ECC71)
	}
}

// ---------- role ops ----------

func (r *Registry) addMutedRoleOnly(guildID, userID string) error {
	// гарантия: перед вызовом все роли (кроме everyone) сняты
	return r.s.GuildMemberRoleAdd(guildID, userID, r.MutedRoleID)
}

func (r *Registry) removeMutedRole(guildID, userID string) error {
	return r.s.GuildMemberRoleRemove(guildID, userID, r.MutedRoleID)
}

func (r *Registry) computeRolesToRemove(guildID, userID string) ([]string, error) {
	m, err := r.s.GuildMember(guildID, userID)
	if err != nil { return nil, err }
	out := make([]string, 0, len(m.Roles))
	for _, rid := range m.Roles {
		if rid == r.MutedRoleID { // если уже есть — всё равно снимем и потом повесим заново
			continue
		}
		out = append(out, rid)
	}
	return out, nil
}

func (r *Registry) dropRoles(guildID, userID string, roles []string) ([]string, error) {
	removed := make([]string, 0, len(roles))
	for _, rid := range roles {
		if err := r.s.GuildMemberRoleRemove(guildID, userID, rid); err != nil {
			// откат уже снятых — чтобы не оставить юзера голым
			_ = r.restoreRoles(guildID, userID, removed)
			return nil, fmt.Errorf("снятие роли %s: %w", rid, err)
		}
		removed = append(removed, rid)
	}
	return removed, nil
}

func (r *Registry) restoreRoles(guildID, userID string, roles []string) error {
	for _, rid := range roles {
		if err := r.s.GuildMemberRoleAdd(guildID, userID, rid); err != nil {
			return fmt.Errorf("возврат роли %s: %w", rid, err)
		}
	}
	return nil
}

// ---------- DB ops ----------

func (r *Registry) hasActiveMute(guildID, userID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var exists bool
	err := r.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM gosha.mutes WHERE guild_id=$1 AND user_id=$2 AND status='active')`,
		guildID, userID,
	).Scan(&exists)
	return exists, err
}

func (r *Registry) insertMuteRow(guildID, userID, moderatorID, reason string, minutes int, removedRoles []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(removedRoles)
	endAt := time.Now().Add(time.Duration(minutes) * time.Minute)
	_, err := r.DB.Exec(ctx, `INSERT INTO gosha.mutes (guild_id, user_id, moderator_id, reason, end_at, duration_minutes, roles_removed, status)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'active')`,
		guildID, userID, moderatorID, reason, endAt, minutes, b,
	)
	return err
}

func (r *Registry) getActiveMute(guildID, userID string) (id int64, roles []string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var jb []byte
	err = r.DB.QueryRow(ctx, 
        `SELECT id, roles_removed FROM gosha.mutes
 WHERE guild_id=$1 AND user_id=$2 AND status='active'
 ORDER BY id DESC LIMIT 1`, 
 guildID, userID).Scan(&id, &jb)
	if err != nil { return 0, nil, err }
	if len(jb) > 0 {
		_ = json.Unmarshal(jb, &roles)
	}
	return id, roles, nil
}

func (r *Registry) completeMute(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.DB.Exec(ctx, `UPDATE gosha.mutes
   SET status='completed', unmuted_at=now(), restored_count=restored_count+1
 WHERE id=$1 AND status='active'`, id)
	return err
}

// ---------- force unmute ----------

func (r *Registry) forceUnmute(guildID, userID, reason string) error {
	if r.DB == nil {
		return fmt.Errorf("DB is nil")
	}
	if reason != "" {
		log.Printf("[mute] forceUnmute user=%s reason=%q", userID, reason)
	}

	id, roles, err := r.getActiveMute(guildID, userID)
	if err != nil { return fmt.Errorf("getActiveMute: %w", err) }

	if err := r.removeMutedRole(guildID, userID); err != nil {
		return fmt.Errorf("removeMutedRole: %w", err)
	}
	if err := r.restoreRoles(guildID, userID, roles); err != nil {
		return fmt.Errorf("restoreRoles: %w", err)
	}
	if err := r.completeMute(id); err != nil {
		return fmt.Errorf("completeMute: %w", err)
	}
	return nil
}


// ---------- perms / hierarchy ----------

func (r *Registry) checkPermissions(ic *discordgo.InteractionCreate, targetUserID string) error {
	perms, err := r.s.UserChannelPermissions(ic.Member.User.ID, ic.ChannelID)
	if err != nil { return fmt.Errorf("не удалось проверить права: %w", err) }
	if perms&discordgo.PermissionAdministrator == 0 && perms&discordgo.PermissionManageRoles == 0 {
		return fmt.Errorf("нужны права Administrator или Manage Roles")
	}
	ok, err := r.botHigherThan(targetUserID)
	if err != nil { return fmt.Errorf("иерархия (target): %w", err) }
	if !ok { return fmt.Errorf("бот ниже цели по иерархии ролей") }
	if ok, err := r.botHigherThanRole(r.MutedRoleID); err != nil {
		return fmt.Errorf("иерархия (mute role): %w", err)
	} else if !ok {
		return fmt.Errorf("роль мута выше роли бота — перетащи мьют-роль НИЖЕ роли бота")
	}
	return nil
}

func (r *Registry) botHigherThan(targetUserID string) (bool, error) {
	g, err := r.s.State.Guild(r.GuildID)
	if err != nil || g == nil {
		g, err = r.s.Guild(r.GuildID)
		if err != nil { return false, err }
	}
	rolePos := func(ids []string) int {
		max := -1
		for _, id := range ids {
			for _, gr := range g.Roles {
				if gr.ID == id && gr.Position > max { max = gr.Position }
			}
		}
		return max
	}
	botID, err := botIDSafe(r.s); if err != nil { return false, err }
	bm, err := r.s.GuildMember(r.GuildID, botID); if err != nil { return false, err }
	tm, err := r.s.GuildMember(r.GuildID, targetUserID); if err != nil { return false, err }
	return rolePos(bm.Roles) > rolePos(tm.Roles), nil
}

func (r *Registry) botHigherThanRole(roleID string) (bool, error) {
	g, err := r.s.State.Guild(r.GuildID)
	if err != nil || g == nil {
		g, err = r.s.Guild(r.GuildID)
		if err != nil { return false, err }
	}
	targetPos := -1
	for _, gr := range g.Roles {
		if gr.ID == roleID { targetPos = gr.Position; break }
	}
	if targetPos == -1 { return false, fmt.Errorf("роль %s не найдена", roleID) }
	botID, err := botIDSafe(r.s); if err != nil { return false, err }
	bm, err := r.s.GuildMember(r.GuildID, botID); if err != nil { return false, err }
	botTop := -1
	for _, id := range bm.Roles {
		for _, gr := range g.Roles {
			if gr.ID == id && gr.Position > botTop { botTop = gr.Position }
		}
	}
	return botTop > targetPos, nil
}

// публичный сеттер, если захочешь поменять срок хранения из main.go
func (r *Registry) SetRetentionDays(days int) {
    if days < 1 { days = 1 }
    r.RetentionDays = days
}

// разовая чистка: удаляет старые неактивные записи
func (r *Registry) cleanupOldMutes() (int64, error) {
    if r.DB == nil { return 0, nil }
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    // удаляем всё, что не active и старше RetentionDays
    cmd, err := r.DB.Exec(ctx, `
        DELETE FROM gosha.mutes
        WHERE status <> 'active'
          AND end_at < now() - ($1 * interval '1 day')`,
        r.RetentionDays,
    )
    if err != nil { return 0, err }
    return cmd.RowsAffected(), nil
}

// ежедневный тикер: запускает cleanup раз в сутки + один прогон на старте
func (r *Registry) startDailyMaintenance() {
    // первый прогон сразу
    if n, err := r.cleanupOldMutes(); err != nil {
        log.Println("[mute] cleanup error:", err)
    } else if n > 0 {
        log.Printf("[mute] cleanup removed %d old rows\n", n)
    }

    // затем — каждые 24 часа
    go func() {
        t := time.NewTicker(24 * time.Hour)
        defer t.Stop()
        for range t.C {
            n, err := r.cleanupOldMutes()
            if err != nil {
                log.Println("[mute] daily cleanup error:", err)
                continue
            }
            if n > 0 {
                log.Printf("[mute] daily cleanup removed %d old rows\n", n)
            }
        }
    }()
}


// ---------- embeds / utils ----------

func (r *Registry) logEmbed(title string, target, moderator *discordgo.User, reason string, color int) {
	if r.LogChannelID == "" {
		log.Println("[mute]", title, "->", userTag(target), "by", userTag(moderator), "reason:", reason)
		return
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: mentionUser(target.ID), Inline: true},
		{Name: "Модератор", Value: mentionUser(moderator.ID), Inline: true},
	}
	if strings.TrimSpace(reason) != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(reason)})
	}
	embed := &discordgo.MessageEmbed{
		Title:     title,
		Color:     color,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer:    &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04"))},
	}
	_, _ = r.s.ChannelMessageSendEmbed(r.LogChannelID, embed)
}

func (r *Registry) logEmbedMute(title string, target, moderator *discordgo.User, reason string, minutes int, color int) {
	extra := fmt.Sprintf("%d мин.", minutes)
	if minutes <= 0 { extra = "не задано" }
	if r.LogChannelID == "" {
		log.Println("[mute]", title, "->", userTag(target), "by", userTag(moderator), "reason:", reason, "minutes:", minutes)
		return
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Пользователь", Value: mentionUser(target.ID), Inline: true},
		{Name: "Модератор", Value: mentionUser(moderator.ID), Inline: true},
		{Name: "Длительность", Value: code(extra), Inline: true},
	}
	if strings.TrimSpace(reason) != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Причина", Value: code(reason)})
	}
	embed := &discordgo.MessageEmbed{
		Title:     title,
		Color:     color,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: avatarURL(target)},
		Fields:    fields,
		Footer:    &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("ID: %s • %s", target.ID, time.Now().Format("02.01.2006 15:04"))},
	}
	_, _ = r.s.ChannelMessageSendEmbed(r.LogChannelID, embed)
}

func (r *Registry) AttachLogger(l *adminlog.Logger) { r.AdminLog = l }

func userTag(u *discordgo.User) string {
	if u == nil { return "—" }
	return fmt.Sprintf("<@%s> (%s)", u.ID, u.Username)
}
func mentionUser(id string) string { return "<@" + id + ">" }
func code(s string) string { return "`" + s + "`" }
func avatarURL(u *discordgo.User) string {
	if u == nil { return "" }
	if u.Avatar != "" { return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=256", u.ID, u.Avatar) }
	return "https://cdn.discordapp.com/embed/avatars/0.png"
}
func botIDSafe(s *discordgo.Session) (string, error) {
	if s.State != nil && s.State.User != nil && s.State.User.ID != "" {
		return s.State.User.ID, nil
	}
	me, err := s.User("@me")
	if err != nil {
		return "", err
	}
	return me.ID, nil
}

// быстрый ACK, чтобы Discord не писал "приложение не отвечает"
func ackEphemeral(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
}

// редактируем уже отправленный deferred-ответ
func editReply(s *discordgo.Session, ic *discordgo.InteractionCreate, msg string) {
	_, _ = s.InteractionResponseEdit(ic.Interaction, &discordgo.WebhookEdit{
		Content: &msg,
	})
}


