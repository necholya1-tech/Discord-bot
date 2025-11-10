package top

import (
	"context"
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry struct {
	DB *pgxpool.Pool
}

func Register(s *discordgo.Session, db *pgxpool.Pool) *Registry {
	r := &Registry{DB: db}
	s.AddHandler(r.onInteractionCreate)
	return r
}

func (r *Registry) onInteractionCreate(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if ic.ApplicationCommandData().Name != "top" {
		return
	}

	// Достаём топ-10 по XP из users_levels
	rows, err := r.DB.Query(context.Background(),
		`SELECT user_id, xp, level
		   FROM users_levels
		  WHERE guild_id = $1
		  ORDER BY xp DESC
		  LIMIT 10`,
		ic.GuildID,
	)
	if err != nil {
		log.Println("[top] query:", err)
		_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Не удалось получить таблицу лидеров.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	defer rows.Close()

	desc := ""
	i := 1
	for rows.Next() {
		var userID string
		var xp, level int64
		if err := rows.Scan(&userID, &xp, &level); err != nil {
			continue
		}
		desc += fmt.Sprintf("**%d.** <@%s> — %d XP (lvl %d)\n", i, userID, xp, level)
		i++
	}
	if desc == "" {
		desc = "Пока пусто. Пиши в чат или сиди в войсе, чтобы зарабатывать XP!"
	}

	embed := &discordgo.MessageEmbed{
		Title:       "🏆 Топ-10 по XP",
		Description: desc,
		Color:       0xFFD700,
	}

	_ = s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	})
}
