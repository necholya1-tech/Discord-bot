package give

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	 "math"

	"gosha_bot/adminlog"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- Константы ролей со спец-логикой (подставь свои ID как в ТЗ) ---
const (
	roleMinus1000XP = "1402166453486096435" // выдача роли => снять 1000xp
	roleMinus1500XP = "1402166685456400445" // выдача роли => снять 1500xp
	roleThirdWarn   = "1404881178720342067" // выдача роли => кикнуть с сервера (3 предупреждение)
)

const CommandName = "give"

type Registry struct {
	GuildID           string
	AdminRoleIDs      map[string]bool
	ProtectedRoleIDs  map[string]bool
	s                 *discordgo.Session
	DB                *pgxpool.Pool
	AdminLog          *adminlog.Logger
	botUserID         string
	botHighestPos     int
	guildRolesByID    map[string]*discordgo.Role
}

// Register — регистрирует команду и хендлер
func Register(
	s *discordgo.Session,
	guildID string,
	adminRoleIDs []string,
	protectedRoleIDs []string,
	db *pgxpool.Pool,
	al *adminlog.Logger,
) (*Registry, error) {

	r := &Registry{
		GuildID:          guildID,
		AdminRoleIDs:     toSet(adminRoleIDs),
		ProtectedRoleIDs: toSet(protectedRoleIDs),
		s:                s,
		DB:               db,
		AdminLog:         al,
	}

	// слушатели
	s.AddHandler(r.onInteractionCreate)

	// Регистрируем команду и считаем позицию бота ТОЛЬКО после Ready
	var readyOnce bool
	s.AddHandler(func(s *discordgo.Session, ev *discordgo.Ready) {
		if readyOnce {
			return
		}
		readyOnce = true

		// bot user id после логина
		r.botUserID = ev.User.ID

		if err := r.refreshGuildRolesCache(); err != nil {
			log.Println("[give] refresh roles:", err)
		}

		cmd := &discordgo.ApplicationCommand{
			Name:        CommandName,
			Description: "Выдать роль пользователю (админ-команда)",
			DefaultMemberPermissions: &[]int64{discordgo.PermissionAdministrator}[0],
			Type:        discordgo.ChatApplicationCommand,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "Кому выдать", Required: true},
				{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Какую роль выдать", Required: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Причина выдачи", Required: true},
			},
		}
		if _, err := s.ApplicationCommandCreate(ev.User.ID, guildID, cmd); err != nil {
			log.Println("[give] cmd create:", err)
		} else {
			log.Println("[give] registered /give")
		}
	})

	return r, nil
}


// onInteractionCreate — обработчик slash-команды
func (r *Registry) onInteractionCreate(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if ic.GuildID != r.GuildID {
		return
	}
	if ic.ApplicationCommandData().Name != CommandName {
		return
	}

	// --- валидация прав: только админы (по ролям из env) ---
	member := ic.Member
	if member == nil {
		return
	}
	if !r.isInvokerAdmin(member.Roles) {
		_ = respondEphemeral(s, ic, "❌ Недостаточно прав. Команда доступна только администраторам.")
		return
	}

	// читаем параметры
	opts := ic.ApplicationCommandData().Options
	var targetUser *discordgo.User
	var roleID, reason string

	for _, o := range opts {
		switch o.Name {
		case "user":
			targetUser = o.UserValue(s)
		case "role":
			role := o.RoleValue(s, ic.GuildID)
			if role != nil {
				roleID = role.ID
			}
		case "reason":
			reason = o.StringValue()
		}
	}

	if targetUser == nil || roleID == "" || reason == "" {
		_ = respondEphemeral(s, ic, "❌ Неверные параметры. Нужно указать user, role и reason.")
		return
	}

	// запрет на защищённые роли
	if r.ProtectedRoleIDs[roleID] {
		_ = respondEphemeral(s, ic, "⛔ Эту роль выдавать нельзя (защищённый список: уровни).")
		return
	}

	// нельзя выдавать роль выше роли бота
	if !r.isRoleBelowBot(roleID) {
		_ = respondEphemeral(s, ic, "⛔ Нельзя выдать роль, которая выше роли бота.")
		return
	}

	// выдаём роль
	if err := s.GuildMemberRoleAdd(r.GuildID, targetUser.ID, roleID); err != nil {
		_ = respondEphemeral(s, ic, fmt.Sprintf("❌ Не удалось выдать роль: %v", err))
		return
	}

	   // спец-логика при выдаче некоторых ролей
eff, err := r.applySideEffects(targetUser.ID, roleID, reason)
if err != nil {
    log.Println("[give] side effects:", err)
}

// лог
r.logGive(targetUser, roleID, reason, ic.Member.User)

// --- красивый embed-ответ ---
embed := &discordgo.MessageEmbed{
    Title: "Выдача роли",
    Color: 0x5865F2,
    Fields: []*discordgo.MessageEmbedField{
        {Name: "Пользователь", Value: fmt.Sprintf("<@%s>", targetUser.ID), Inline: true},
        {Name: "Роль", Value: fmt.Sprintf("<@&%s>", roleID), Inline: true},
        {Name: "Причина", Value: reason, Inline: false},
    },
}

if eff != nil {
    xpLine := fmt.Sprintf("%d → %d", eff.XPBefore, eff.XPAfter)
    lvLine := fmt.Sprintf("%d → %d", eff.LevelBefore, eff.LevelAfter)
    if eff.XPBefore == 0 && eff.LevelBefore == 0 && (eff.XPAfter != 0 || eff.LevelAfter != 0) {
        xpLine = fmt.Sprintf("%d", eff.XPAfter)
        lvLine = fmt.Sprintf("%d", eff.LevelAfter)
    }
    embed.Fields = append(embed.Fields,
        &discordgo.MessageEmbedField{Name: "XP", Value: xpLine, Inline: true},
        &discordgo.MessageEmbedField{Name: "Уровень", Value: lvLine, Inline: true},
    )
    if eff.RemovedRoleID != "" {
        embed.Fields = append(embed.Fields,
            &discordgo.MessageEmbedField{Name: "Снята роль", Value: fmt.Sprintf("<@&%s>", eff.RemovedRoleID), Inline: true},
        )
    }
    if eff.Kicked {
        embed.Fields = append(embed.Fields,
            &discordgo.MessageEmbedField{Name: "Действие", Value: "Пользователь кикнут (3-е предупреждение)", Inline: false},
        )
    }
}

_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
    Type: discordgo.InteractionResponseChannelMessageWithSource,
    Data: &discordgo.InteractionResponseData{
        Flags:  discordgo.MessageFlagsEphemeral,
        Embeds: []*discordgo.MessageEmbed{embed},
    },
})
}

// --- helpers ---
func (r *Registry) isInvokerAdmin(invokerRoleIDs []string) bool {
	for _, rid := range invokerRoleIDs {
		if r.AdminRoleIDs[rid] {
			return true
		}
	}
	return false
}


func (r *Registry) refreshGuildRolesCache() error {
	roles, err := r.s.GuildRoles(r.GuildID)
	if err != nil {
		return err
	}
	r.guildRolesByID = make(map[string]*discordgo.Role, len(roles))

	// найдём максимальную позицию роли бота
	r.botHighestPos = -1

	// у бота нет member в payload здесь — достанем его member с ролями
	botMember, err := r.s.GuildMember(r.GuildID, r.botUserID)
	if err != nil {
		return err
	}
	rolePos := make(map[string]int, len(roles))
	for _, role := range roles {
		r.guildRolesByID[role.ID] = role
		rolePos[role.ID] = role.Position
	}
	for _, rid := range botMember.Roles {
		if p := rolePos[rid]; p > r.botHighestPos {
			r.botHighestPos = p
		}
	}
	return nil
}

func (r *Registry) isRoleBelowBot(targetRoleID string) bool {
	role := r.guildRolesByID[targetRoleID]
	if role == nil {
		// на всякий случай обновим кэш и проверим ещё раз
		if err := r.refreshGuildRolesCache(); err != nil {
			log.Println("[give] refresh cache failed:", err)
			return false
		}
		role = r.guildRolesByID[targetRoleID]
		if role == nil {
			return false
		}
	}
	return role.Position < r.botHighestPos
}

// --- XP/Level helpers ---

func (r *Registry) getUserStats(userID string) (xp int64, level int, err error) {
	if r.DB == nil {
		return 0, 1, fmt.Errorf("DB not configured")
	}
	err = r.DB.QueryRow(
		context.Background(),
		`SELECT xp, level FROM users_levels WHERE guild_id=$1 AND user_id=$2`,
		r.GuildID, userID,
	).Scan(&xp, &level)
	// если строки нет — считаем начальные значения
	if err != nil {
		return 0, 1, nil
	}
	return
}

func levelFromXP(xp int64) int {
	if xp < 0 {
		xp = 0
	}
	// threshold(L)=10*L^2 → L=floor(sqrt(xp/10)), но минимум 1
	l := int(math.Floor(math.Sqrt(float64(xp) / 10.0)))
	if l < 1 {
		l = 1
	}
	return l
}

func (r *Registry) setXPAndRecalc(userID string, newXP int64) (xpAfter int64, levelAfter int, err error) {
	if r.DB == nil {
		return 0, 1, fmt.Errorf("DB not configured")
	}
	if newXP < 0 {
		newXP = 0
	}
	newLvl := levelFromXP(newXP)
	_, err = r.DB.Exec(
		context.Background(),
		`UPDATE users_levels SET xp=$1, level=$2, updated_at=now()
         WHERE guild_id=$3 AND user_id=$4`,
		newXP, newLvl, r.GuildID, userID,
	)
	return newXP, newLvl, err
}

// снимает amount и возвращает ДО/ПОСЛЕ
func (r *Registry) deductXP(userID string, amount int) (xpBefore int64, lvlBefore int, xpAfter int64, lvlAfter int, err error) {
	xpBefore, lvlBefore, err = r.getUserStats(userID)
	if err != nil {
		return
	}
	newXP := xpBefore - int64(amount)
	xpAfter, lvlAfter, err = r.setXPAndRecalc(userID, newXP)
	return
}


type Effect struct {
	XPBefore, XPAfter   int64
	LevelBefore, LevelAfter int
	RemovedRoleID       string
	Kicked              bool
}

func (r *Registry) applySideEffects(userID, roleID, reason string) (*Effect, error) {
	e := &Effect{}

	switch roleID {
	case roleMinus1000XP:
		var err error
		e.XPBefore, e.LevelBefore, e.XPAfter, e.LevelAfter, err = r.deductXP(userID, 1000)
		return e, err

	case roleMinus1500XP:
		var err error
		e.XPBefore, e.LevelBefore, e.XPAfter, e.LevelAfter, err = r.deductXP(userID, 1500)
		if err != nil {
			return e, err
		}
		// снять предыдущую предупреждающую роль, если есть
		if remErr := r.s.GuildMemberRoleRemove(r.GuildID, userID, roleMinus1000XP); remErr == nil {
			e.RemovedRoleID = roleMinus1000XP
		}
		return e, nil

	case roleThirdWarn:
		// обнуляем XP и уровень → 1, потом кикаем
		if _, _, err := r.setXPAndRecalc(userID, 0); err != nil {
			return e, err
		}
		kickReason := fmt.Sprintf("3-е предупреждение: %s (выдана роль %s)", reason, roleID)
		if err := r.s.GuildMemberDeleteWithReason(r.GuildID, userID, kickReason); err != nil {
			return e, err
		}
		e.Kicked = true
		// для отчёта:
		e.XPBefore, e.LevelBefore, _ = r.getUserStats(userID) // мог отсутствовать
		e.XPAfter, e.LevelAfter = 0, 1
		return e, nil

	default:
		return nil, nil
	}
}

func (r *Registry) logGive(target *discordgo.User, roleID, reason string, exec *discordgo.User) {
	msg := fmt.Sprintf(
		"Выдача роли: <@%s> получил(а) <@&%s>\nПричина: %s\nВыдал: <@%s>",
		target.ID, roleID, reason, exec.ID,
	)

	// TODO: если есть подходящий метод в adminlog.Logger — замени println на него
	log.Println("[give]", msg)
}


func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			m[id] = true
		}
	}
	return m
}

// Вспомогательный ответ-эпемерал
func respondEphemeral(s *discordgo.Session, ic *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: content,
		},
	})
}

// --- Утилиты для чтения ENV (если пригодится где-то ещё) ---

func ParseCSVEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
