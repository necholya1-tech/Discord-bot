package remove

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"gosha_bot/adminlog"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CommandName = "remove"

type Registry struct {
	guildID          string
	s                *discordgo.Session
	adminRoleIDs     []string                    // кто может вызывать /remove
	protectedRoleIDs map[string]struct{}         // роли, которые нельзя снимать (level-ролы)
	DB               *pgxpool.Pool               // если не nil — подгружает protected из БД
	AdminLog         *adminlog.Logger            // опционально
}

// Register регистрирует команду и настраивает обработчики.
// adminRoleIDs — список ролей, которым разрешено снимать роли.
// protectedRoleIDs — дополнительно защищённые роли (если DB=nil, используется этот список).
func Register(
	s *discordgo.Session,
	guildID string,
	adminRoleIDs []string,
	protectedRoleIDs []string,
	db *pgxpool.Pool,
	logger *adminlog.Logger,
) (*Registry, error) {
	r := &Registry{
		guildID:          guildID,
		s:                s,
		adminRoleIDs:     dedup(adminRoleIDs),
		protectedRoleIDs: make(map[string]struct{}),
		DB:               db,
		AdminLog:         logger,
	}

	// заполнить защищённые роли из .env
	for _, id := range protectedRoleIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			r.protectedRoleIDs[id] = struct{}{}
		}
	}
	// если есть БД — подтянуть level-роли (roles.role_id); логируем warn при ошибке и идём дальше
	if r.DB != nil {
		if err := r.ReloadProtectedRolesFromDB(context.Background()); err != nil {
			log.Println("[remove] warn: failed to load level roles from DB:", err)
		}
	}

	// описываем слэш-команду
	cmd := &discordgo.ApplicationCommand{
		Name:        CommandName,
		Description: "Снять указанную роль с пользователя",
		Type:        discordgo.ChatApplicationCommand,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "Кому снять роль", Required: true},
			{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Какую роль снять", Required: true},
			{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Причина (необязательно)"},
		},
	}

	// Регистрируем команду, когда сессия полностью готова (есть rdy.User.ID)
	s.AddHandlerOnce(func(s *discordgo.Session, rdy *discordgo.Ready) {
		if _, err := s.ApplicationCommandCreate(rdy.User.ID, guildID, cmd); err != nil {
			log.Println("[remove] create cmd error:", err)
		} else {
			log.Println("[remove] /remove registered")
		}
	})

	// обработчик интеракций можно вешать сразу
	s.AddHandler(r.onInteraction)

	return r, nil
}


// ReloadProtectedRolesFromDB подтягивает роли из таблицы roles (role_id), чтобы нельзя было их снимать.
func (r *Registry) ReloadProtectedRolesFromDB(ctx context.Context) error {
	return nil
}

func (r *Registry) onInteraction(s *discordgo.Session, ev *discordgo.InteractionCreate) {
	ic := ev.Interaction
	if ic.Type != discordgo.InteractionApplicationCommand || ic.GuildID != r.guildID {
		return
	}
	if ic.ApplicationCommandData().Name != CommandName {
		return
	}

	// Ответим сразу, чтобы Discord не ждал (ephemeral).
	_ = s.InteractionRespond(ic, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6}, // ephemeral
	})

	// Валидации
	if ic.Member == nil {
		r.followup(ic, "Команда доступна только внутри сервера.")
		return
	}
	if !r.isAdmin(ic.Member) {
		r.followup(ic, "⛔ Команда доступна только администраторам.")
		return
	}

	opts := ic.ApplicationCommandData().Options
	var targetUser *discordgo.User
	var role *discordgo.Role
	var reason string

	for _, o := range opts {
		switch o.Name {
		case "user":
			targetUser = o.UserValue(s)
		case "role":
			role = o.RoleValue(s, ic.GuildID)
		case "reason":
			reason = o.StringValue()
		}
	}
	if targetUser == nil || role == nil {
		r.followup(ic, "Не удалось прочитать параметры (user/role).")
		return
	}

	// Нельзя снимать защищённые (уровневые) роли
	if r.isProtected(role.ID) {
		r.followup(ic, "🔒 Нельзя снимать уровневые роли.")
		return
	}

	// Проверим иерархию: роль должна быть ниже самой высокой роли бота
	ok, err := r.botHigherThan(role.ID)
	if err != nil {
		log.Println("[remove] botHigherThan error:", err)
		r.followup(ic, "Ошибка проверки иерархии ролей.")
		return
	}
	if !ok {
		r.followup(ic, "⛔ Нельзя снимать роль, которая не ниже роли бота по иерархии.")
		return
	}

	// Проверим, есть ли у пользователя эта роль
	member, err := s.GuildMember(ic.GuildID, targetUser.ID)
	if err != nil {
		r.followup(ic, "Не удалось получить участника.")
		return
	}
	if !slices.Contains(member.Roles, role.ID) {
		r.followup(ic, "У пользователя нет этой роли.")
		return
	}

	// Попробуем снять
	if err := s.GuildMemberRoleRemove(ic.GuildID, targetUser.ID, role.ID); err != nil {
		log.Println("[remove] remove error:", err)
		r.followup(ic, "Не удалось снять роль. У бота достаточно прав и включён ли `Manage Roles`?")
		return
	}

	// Лог
	if r.AdminLog != nil {
		r.logRemoval(
    fmt.Sprintf(
        "Пользователь: <@%s>\nРоль: <@&%s>\nМодератор: <@%s>\nПричина: %s",
        targetUser.ID, role.ID, ic.Member.User.ID, emptyIf(reason, "—"),
    ),
)

	r.followup(ic, fmt.Sprintf("✅ Роль <@&%s> снята с <@%s>.", role.ID, targetUser.ID))
}
}

func (r *Registry) isAdmin(m *discordgo.Member) bool {
	if m == nil {
		return false
	}
	if len(r.adminRoleIDs) == 0 {
		return false
	}
	for _, have := range m.Roles {
		for _, need := range r.adminRoleIDs {
			if have == need {
				return true
			}
		}
	}
	return false
}

func (r *Registry) isProtected(roleID string) bool {
	_, ok := r.protectedRoleIDs[roleID]
	return ok
}

// botHigherThan проверяет, что целевая роль ниже самой высокой роли бота.
func (r *Registry) botHigherThan(targetRoleID string) (bool, error) {
	// Получим роли сервера
	roles, err := r.s.GuildRoles(r.guildID)
	if err != nil {
		return false, err
	}
	rolePos := make(map[string]int)
	for _, rr := range roles {
		rolePos[rr.ID] = rr.Position
	}

	// Получим участника-бота на сервере
	appID := r.s.State.User.ID
	botMember, err := r.s.GuildMember(r.guildID, appID)
	if err != nil {
		return false, err
	}

	// Найдём максимальную позицию среди ролей бота
	maxBotPos := -1
	for _, rid := range botMember.Roles {
		if p, ok := rolePos[rid]; ok && p > maxBotPos {
			maxBotPos = p
		}
	}
	// Позиция целевой роли
	tPos, ok := rolePos[targetRoleID]
	if !ok {
		return false, fmt.Errorf("target role not found in guild roles")
	}

	// Бот должен быть строго выше
	return maxBotPos > tPos, nil
}

// --- helpers for adminlog compatibility ---

type loggerWithPostSimplef interface {
	PostSimplef(title, format string, args ...any) error
}
type loggerWithPost interface {
	Post(title, text string) error
}
type loggerWithPrintf interface {
	Printf(format string, args ...any)
}
type loggerWithInfof interface {
	Infof(format string, args ...any)
}

// logRemoval отправляет запись в adminlog, поддерживая разные API логгера.
// Если подходящего метода нет — пишет в стандартный лог.
func (r *Registry) logRemoval(text string) {
	const title = "Снятие роли"

	if r.AdminLog == nil {
		log.Println("[remove]", title+"\n"+text)
		return
	}

	switch l := any(r.AdminLog).(type) {
	case loggerWithPostSimplef:
		_ = l.PostSimplef(title, "%s", text)
	case loggerWithPost:
		_ = l.Post(title, text)
	case loggerWithPrintf:
		l.Printf("%s\n%s", title, text)
	case loggerWithInfof:
		l.Infof("%s\n%s", title, text)
	default:
		log.Println("[remove]", title+"\n"+text)
	}
}


func (r *Registry) followup(ic *discordgo.Interaction, content string) {
	_, _ = r.s.FollowupMessageCreate(ic, true, &discordgo.WebhookParams{
		Content: content,
	})
}

func dedup(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func emptyIf(s, repl string) string {
	if strings.TrimSpace(s) == "" {
		return repl
	}
	return s
}
