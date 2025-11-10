package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"math"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"gosha_bot/adminlog"
	"gosha_bot/clear"
	"gosha_bot/level"
	"gosha_bot/mute"
	"gosha_bot/selfrole"
	"gosha_bot/remove"
	"gosha_bot/top"
	"gosha_bot/give"
)

func main() {
	var registerOnce sync.Once

	_ = godotenv.Load(".env")

	must := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			log.Fatal(k, " is empty")
		}
		return v
	}

	// ENV
	token := must("DISCORD_TOKEN")
	guildID := must("GUILD_ID")
	if guildID == "" {
    log.Fatal("GUILD_ID is empty")
}
	muteRoleID := must("MUTE_ROLE_ID")
	logChID := os.Getenv("ADMIN_LOG_CHANNEL_ID")
	keepCatID := os.Getenv("KEEP_CATEGORY_ID")

	fmt.Println("[env dbg] DISCORD_TOKEN length:", len(token))
	log.Println("[dbg] cwd:", func() string { d, _ := os.Getwd(); return d }())
	for _, k := range []string{"GUILD_ID", "WELCOME_CHANNEL_ID", "SELF_ROLE_ID", "MUTE_ROLE_ID", "ADMIN_LOG_CHANNEL_ID", "KEEP_CATEGORY_ID"} {
		log.Printf("[env dbg] %s=%q", k, os.Getenv(k))
	}

	// Session + intents
	intents := discordgo.IntentsGuilds |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildBans |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildVoiceStates

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("discord session:", err)
	}
	s.Identify.Intents = intents
	log.Printf("[dbg] intents mask: %d", s.Identify.Intents)

	// init selfrole + clear
	if err := selfrole.Init(s); err != nil {
		log.Fatal("selfrole init:", err)
	}
	clear.AddHandler(s)

	// admin log
	adm := adminlog.Init(s, guildID, logChID)

	// DB
	var pool *pgxpool.Pool
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		pool = mustDBPool(dsn)
		defer pool.Close()
	} else {
		log.Println("[warn] POSTGRES_DSN is empty — DB features limited")
	}
	

	// mute
	mr, err := mute.Register(s, guildID, keepCatID, logChID, muteRoleID, pool)
	if err != nil {
		log.Fatal("mute register:", err)
	}
	mr.AttachLogger(adm)
	mr.SetRetentionDays(3)

	// level
	_, err = level.Register(s, guildID, pool, level.RolesConfig{
		RoleL1to24:   "1401993276730380531",
		RoleL25to49:  "1401993388345262133",
		RoleL50to74:  "1401993503420190760",
		RoleL75to99:  "1401993577495527534",
		RoleL100Plus: "1401993637839245434",
	}, "636654459682029578") // AFK
	if err != nil {
		log.Fatal("level.Register:", err)
	}

	wireRemove(s, guildID, pool, adm)
	wireGive(s, guildID, pool, adm)


	// --- /level (EMBED) ---
	s.AddHandler(func(s *discordgo.Session, ic *discordgo.InteractionCreate) {
		if ic.Type != discordgo.InteractionApplicationCommand {
			return
		}
		data := ic.ApplicationCommandData()
		if data.Name != "level" {
			return
		}

		// чей уровень
		targetID := ic.Member.User.ID
		targetTag := ic.Member.User.Username
		if len(data.Options) > 0 && data.Options[0].Type == discordgo.ApplicationCommandOptionUser {
			if u := data.Options[0].UserValue(s); u != nil {
				targetID = u.ID
				targetTag = u.Username
			}
		}

		// из БД
		var xp int64 = 0
		var lvl int = 1
		if pool != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = pool.QueryRow(ctx,
				`SELECT xp, level FROM users_levels WHERE guild_id=$1 AND user_id=$2`,
				guildID, targetID,
			).Scan(&xp, &lvl)
		}

				// пороги (10*L^2)
		prev := int64(10 * lvl * lvl)
		next := int64(10 * (lvl+1) * (lvl+1))

		// клампим xp в [prev, next] и защищаемся от деления на 0
		if next <= prev {
			next = prev + 1
		}
		if xp < prev {
			xp = prev
		}
		if xp > next {
			xp = next
		}

		need := next - xp
		prog := float64(xp-prev) / float64(next-prev) // 0..1

		// прогресс-бар на 10 клеток c округлением (а не усечением)
		const cells = 10
		filled := int(math.Round(prog * float64(cells)))
		if filled < 0 {
			filled = 0
		}
		if filled > cells {
			filled = cells
		}

		// используем эмодзи одинаковой ширины
		var bar strings.Builder
		for i := 0; i < cells; i++ {
			if i < filled {
				bar.WriteString("🟩")
			} else {
				bar.WriteString("⬜")
			}
		}
		percent := int(math.Round(prog * 100))



		// аватар
		thumb := ""
		if u, _ := s.User(targetID); u != nil {
			thumb = discordgo.EndpointUserAvatar(u.ID, u.Avatar)
		}

		// тир-роль
		tier := func(level int) string {
			switch {
			case level >= 100:
				return "<@&1401993637839245434>"
			case level >= 75:
				return "<@&1401993577495527534>"
			case level >= 50:
				return "<@&1401993503420190760>"
			case level >= 25:
				return "<@&1401993388345262133>"
			default:
				return "<@&1401993276730380531>"
			}
		}

		embed := &discordgo.MessageEmbed{
			Title:       "Уровень и опыт",
			Description: fmt.Sprintf("**%s**", targetTag),
			Color:       0x5865F2,
			Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: thumb},
			Fields: []*discordgo.MessageEmbedField{
				{Name: "Уровень", Value: fmt.Sprintf("%d", lvl), Inline: true},
				{Name: "XP", Value: fmt.Sprintf("%d", xp), Inline: true},
				{Name: "Тир-роль", Value: tier(lvl), Inline: true},
			    {Name: "Прогресс", Value: fmt.Sprintf("%s  %d%%", bar.String(), percent), Inline: false},
				{Name: "До следующего", Value: fmt.Sprintf("%d XP → lvl %d", need, lvl+1), Inline: true},
			},
			Footer: &discordgo.MessageEmbedFooter{
				Text: "Войс: 100 XP/час",
			},
		}

		_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{embed},
			},
		})
	})

	// --- /welcome (служебная) ---
	s.AddHandler(func(s *discordgo.Session, ic *discordgo.InteractionCreate) {
		if ic.Type != discordgo.InteractionApplicationCommand {
			return
		}
		if ic.ApplicationCommandData().Name != "welcome" {
			return
		}
		_ = selfrole.SendWelcome(s, ic.Member.User.ID)
		_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Отправил сообщение в welcome-канал.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	})

	// Регистрация slash-команд после Ready
	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		registerOnce.Do(func() {
			log.Println("[cmd] registering slash commands…")
			appID := r.User.ID

			cmds := []*discordgo.ApplicationCommand{
				{
					Name:        "mute",
					Description: "Выдать мут пользователю на N минут",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "Кому выдать мут", Required: true},
						{Type: discordgo.ApplicationCommandOptionInteger, Name: "minutes", Description: "На сколько минут", Required: true, MinValue: func() *float64 { v := 1.0; return &v }()},
						{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Причина", Required: false},
					},
				},
				{
					Name:        "unmute",
					Description: "Снять мут с пользователя",
					Options: []*discordgo.ApplicationCommandOption{
						{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "С кого снять мут", Required: true},
						{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Причина", Required: false},
					},
				},
				{
					Name:        "clear",
					Description: "Удалить последние N сообщений в этом канале",
					Options: []*discordgo.ApplicationCommandOption{
						func() *discordgo.ApplicationCommandOption {
							opt := &discordgo.ApplicationCommandOption{
								Type:        discordgo.ApplicationCommandOptionInteger,
								Name:        "count",
								Description: "Сколько (1–100)",
								Required:    true,
							}
							clear.SetMinMaxRange(opt, 1, 100)
							return opt
						}(),
					},
				},
				{
					Name:        "level",
					Description: "Показать уровень и XP (свой или другого пользователя)",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionUser,
							Name:        "user",
							Description: "Пользователь (по умолчанию — ты)",
							Required:    false,
						},
					},
				},
				{
                           Name:        "top",
                           Description: "Показать топ-10 пользователей по XP",
                },
			}

			for _, c := range cmds {
				if _, err := s.ApplicationCommandCreate(appID, guildID, c); err != nil {
					log.Println("[cmd] create:", c.Name, err)
				}
			}
			log.Println("[cmd] registered")
		})
	})

    top.Register(s, pool)

	// запуск
	if err := s.Open(); err != nil {
		log.Fatal("open gateway:", err)
	}
	defer s.Close()

	log.Println("Bot is up")
	select {}
}

func mustDBPool(dsn string) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = 5

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	return pool
}

func mustSliceEnv(key string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	// поддержка "id1,id2,id3" или с пробелами
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func wireRemove(s *discordgo.Session, guildID string, pool *pgxpool.Pool, logger *adminlog.Logger) {
    adminRoleIDs := mustSliceEnv("ADMIN_ROLE_IDS")
    protected    := mustSliceEnv("PROTECTED_ROLE_IDS")

    _, err := remove.Register(s, guildID, adminRoleIDs, protected, pool, logger)
    if err != nil {
        log.Fatal("remove.Register:", err)
    }
}

func wireGive(s *discordgo.Session, guildID string, pool *pgxpool.Pool, logger *adminlog.Logger) {
    adminRoleIDs := mustSliceEnv("ADMIN_ROLE_IDS")
    protected    := mustSliceEnv("PROTECTED_ROLE_IDS")

    _, err := give.Register(s, guildID, adminRoleIDs, protected, pool, logger)
    if err != nil {
        log.Fatal("give.Register:", err)
    }
}


