package clear

import (
	"log"
	"reflect"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

const CommandName = "clear"
const maxBulk = 100 // лимит Discord на bulk delete

// Register — регистрирует slash-команду /clear на сервере.
// Работает с разными версиями discordgo (MinValue/MaxValue: float64 ИЛИ *float64).
func Register(s *discordgo.Session, guildID string) (*discordgo.ApplicationCommand, error) {
	var manageMessagesPerm int64 = discordgo.PermissionManageMessages
	dm := false

    //создаем опцию команды (аргумент /clear)
	opt := &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionInteger,
		Name:        "count",
		Description: "Сколько сообщений удалить (1–100)",
		Required:    true,
	}
	setMinMax(opt, 1, 100)

	cmd := &discordgo.ApplicationCommand{
		Name:        CommandName,
		Description: "Удалить последние N сообщений в этом канале",
		DefaultMemberPermissions: &manageMessagesPerm,
		DMPermission:             &dm,
		Options:                  []*discordgo.ApplicationCommandOption{opt},
	}
	return s.ApplicationCommandCreate(s.State.User.ID, guildID, cmd)
}

// AddHandler — обработчик выполнения команды
func AddHandler(s *discordgo.Session) {
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand || i.ApplicationCommandData().Name != CommandName {
			return
		}

		channelID := i.ChannelID
		count := int(i.ApplicationCommandData().Options[0].IntValue())
		if count < 1 {
			count = 1
		}
		if count > maxBulk {
			count = maxBulk
		}

		deleted, err := deleteLastMessages(s, channelID, count)
		if err != nil {
			log.Println("[clear] delete error:", err)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags:   discordgo.MessageFlagsEphemeral,
					Content: "Не удалось удалить сообщения: " + err.Error(),
				},
			})
			return
		}

		msg := "Удалено сообщений: " + strconv.Itoa(deleted)
		if deleted == 1 {
			msg = "Удалено 1 сообщение."
		} else if deleted == 0 {
			msg = "Ничего не удалено."
		}

		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Flags:   discordgo.MessageFlagsEphemeral,
				Content: msg,
			},
		})
	})
}

// deleteLastMessages — удаляет N последних сообщений.
// Новые (моложе 14 дней) пробуем удалить пачкой; остальные — по одному.
func deleteLastMessages(s *discordgo.Session, channelID string, n int) (int, error) {
	msgs, err := s.ChannelMessages(channelID, n, "", "", "")
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	var bulkIDs, oldIDs []string
	for _, m := range msgs {
		if m.Timestamp.After(cutoff) {
			bulkIDs = append(bulkIDs, m.ID)
		} else {
			oldIDs = append(oldIDs, m.ID)
		}
	}

	deleted := 0

	// bulk (нельзя, если только 1 id)
	if len(bulkIDs) > 1 {
		if err := s.ChannelMessagesBulkDelete(channelID, bulkIDs); err == nil {
			deleted += len(bulkIDs)
		} else {
			for _, id := range bulkIDs {
				if err := s.ChannelMessageDelete(channelID, id); err == nil {
					deleted++
					time.Sleep(350 * time.Millisecond)
				}
			}
		}
	} else if len(bulkIDs) == 1 {
		if err := s.ChannelMessageDelete(channelID, bulkIDs[0]); err == nil {
			deleted++
			time.Sleep(350 * time.Millisecond)
		}
	}

	// старые сообщения — только по одному
	for _, id := range oldIDs {
		if err := s.ChannelMessageDelete(channelID, id); err == nil {
			deleted++
			time.Sleep(350 * time.Millisecond)
		}
	}

	return deleted, nil
}

// setMinMax ставит MinValue/MaxValue вне зависимости от того, pointer там или float64.
func setMinMax(opt *discordgo.ApplicationCommandOption, min, max float64) {
	v := reflect.ValueOf(opt).Elem()

	if f := v.FieldByName("MinValue"); f.IsValid() && f.CanSet() {
		switch f.Kind() {
		case reflect.Float64:
			f.SetFloat(min)
		case reflect.Ptr:
			if f.Type().Elem().Kind() == reflect.Float64 {
				mv := min
				f.Set(reflect.ValueOf(&mv))
			}
		}
	}

	if f := v.FieldByName("MaxValue"); f.IsValid() && f.CanSet() {
		switch f.Kind() {
		case reflect.Float64:
			f.SetFloat(max)
		case reflect.Ptr:
			if f.Type().Elem().Kind() == reflect.Float64 {
				mv := max
				f.Set(reflect.ValueOf(&mv))
			}
		}
	}
}

func SetMinMaxRange(opt *discordgo.ApplicationCommandOption, min, max float64) {
	setMinMax(opt, min, max)
}
