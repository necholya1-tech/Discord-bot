package selfrole

import (
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
)

var (
	guildID          string
	welcomeChannelID string
	selfRoleID       string
)

const btnID = "selfrole:grant"

func Init(s *discordgo.Session) error {
	// читаем env
	guildID = os.Getenv("GUILD_ID")
	welcomeChannelID = os.Getenv("WELCOME_CHANNEL_ID")
	selfRoleID = os.Getenv("SELF_ROLE_ID")

	if guildID == "" || welcomeChannelID == "" || selfRoleID == "" {
		return ErrEnvNotSet
	}

	// хэндлеры
	s.AddHandler(onMemberJoin)
	s.AddHandler(onButton)

	return nil
}

var ErrEnvNotSet = &envErr{"GUILD_ID / WELCOME_CHANNEL_ID / SELF_ROLE_ID must be set"}

type envErr struct{ msg string }
func (e *envErr) Error() string { return e.msg }

// Отправляем сообщение с кнопкой при входе участника
func onMemberJoin(s *discordgo.Session, e *discordgo.GuildMemberAdd) {
	if e.GuildID != guildID{
		return
	}

	if err:= SendWelcome(s, e.User.ID); err!=nil{
		log.Println("[selfrole] welcome send error:", err)
	}
}

// Выдаём роль по клику на кнопку
func onButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	if i.MessageComponentData().CustomID != btnID {
		return
	}
	if i.GuildID != guildID {
		return
	}

	userID := i.Member.User.ID

	// Добавляем роль (идемпотентно — Discord просто вернёт 204/ошибку, если уже есть)
	err := s.GuildMemberRoleAdd(guildID, userID, selfRoleID)
	if err != nil {
		log.Println("[selfrole] add role error:", err)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Не смог выдать роль. Проверьте права бота и порядок ролей.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Эфемерное подтверждение
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Готово! Роль выдана.",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func SendWelcome(s *discordgo.Session, userID string) error {
	//создаем приватный тред в велком канале
	th,err:= s.ThreadStartComplex(welcomeChannelID,&discordgo.ThreadStart{
		Name: "Приветствие"+ userID,
		AutoArchiveDuration: 60, //архив через час
		Type: discordgo.ChannelTypeGuildPrivateThread,
		Invitable: false,
	})
	if err !=nil{
		return err
	}

	//добавляем юзера в тред 
	if err:= s.ThreadMemberAdd(th.ID, userID); err!=nil{
		return err
	}

	//отправляем кнопку 
	_,err= s.ChannelMessageSendComplex(th.ID, &discordgo.MessageSend{
		Content: "👋 Добро пожаловать! Нажми кнопку, чтобы получить роль.",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						CustomID: btnID,
						Label:"Получить роль",
						Style: discordgo.PrimaryButton,
					},
				},
			},
		},
	})
	return err
}
