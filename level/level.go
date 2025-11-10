package level

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry struct {
	s            *discordgo.Session
	DB           *pgxpool.Pool
	GuildID      string
	AfkChannelID string

	// role tiers
	roleL1to24   string
	roleL25to49  string
	roleL50to74  string
	roleL75to99  string
	roleL100Plus string

	muVoice   sync.Mutex
	voiceJoin map[string]time.Time // userID -> join time (если не AFK)
}

type RolesConfig struct {
	RoleL1to24   string
	RoleL25to49  string
	RoleL50to74  string
	RoleL75to99  string
	RoleL100Plus string
}

func Register(s *discordgo.Session, guildID string, db *pgxpool.Pool, rc RolesConfig, afkChannelID string) (*Registry, error) {
	r := &Registry{
		s:            s,
		DB:           db,
		GuildID:      guildID,
		AfkChannelID: afkChannelID,

		roleL1to24:   rc.RoleL1to24,
		roleL25to49:  rc.RoleL25to49,
		roleL50to74:  rc.RoleL50to74,
		roleL75to99:  rc.RoleL75to99,
		roleL100Plus: rc.RoleL100Plus,

		voiceJoin: make(map[string]time.Time),
	}

	s.AddHandler(r.onMessageCreate)
s.AddHandler(r.onVoiceStateUpdate)

go func() {
    ticker := time.NewTicker(1 * time.Minute) // 1m в проде; можно 10s в тесте
    defer ticker.Stop()

    for range ticker.C {
        now := time.Now().UTC()

        // 1) Снимем копию карты под мьютексом (min критическая секция)
        r.muVoice.Lock()
        snapshot := make(map[string]time.Time, len(r.voiceJoin))
        for uid, from := range r.voiceJoin {
            snapshot[uid] = from
        }
        r.muVoice.Unlock()

        // 2) Обрабатываем начисление XP без мьютекса (можно ходить в БД)
        updates := make(map[string]time.Time, len(snapshot))
        for uid, from := range snapshot {
            if newFrom, added := r.addVoiceXPWithCarry(uid, from, now); added {
                updates[uid] = newFrom
            }
        }

        // 3) Возвращаем новые "from" под коротким локом
        if len(updates) > 0 {
            r.muVoice.Lock()
            for uid, newFrom := range updates {
                // Пользователь мог за это время уйти/перейти канал — проверим, что он ещё в карте
                if _, ok := r.voiceJoin[uid]; ok {
                    // (опционально) можно убедиться, что старт не менялся:
                    // if r.voiceJoin[uid] == snapshot[uid] { r.voiceJoin[uid] = newFrom }
                    r.voiceJoin[uid] = newFrom
                }
            }
            r.muVoice.Unlock()
        }
    }
}()   // конец горутины

return r, nil
}     // конец функции Register

// ====== Математика уровней: threshold(L) = 3*L^2 ======
func xpToLevel(xp int64) int {
    if xp <= 0 { return 1 }
    l := int(math.Floor(math.Sqrt(float64(xp) / 10.0)))
    if l < 1 { l = 1 }
    return l
}
// ====== Сообщения: +1 XP раз в минуту ======

func (r *Registry) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
    if m.GuildID != r.GuildID { return }
    if m.Author == nil || m.Author.Bot { return }

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()

    u, err := UpsertUser(ctx, r.DB, r.GuildID, m.Author.ID, m.Author.Username, nickFromMember(m.Member))
    if err != nil { log.Println("[level] upsert user err:", err); return }

    now := time.Now().UTC()
    if u.LastMsgAt != nil && now.Sub(*u.LastMsgAt) < time.Minute { return }

    newXP := u.XP + 1
    newLevel := xpToLevel(newXP)

    if err := UpdateAfterMessage(ctx, r.DB, r.GuildID, m.Author.ID, newXP, &now, newLevel); err != nil {
        log.Println("[level] UpdateAfterMessage err:", err); return
    }

    // (опционально) звать applyLevelRoles только если newLevel != u.Level
    if newLevel != u.Level {
        if err := r.applyLevelRoles(m.Author.ID, newLevel); err != nil {
            log.Println("[level] apply roles (message) err:", err)
        }
    }
}

// ====== Войс: 100 XP за час (пропорционально времени), игнор AFK ======

func (r *Registry) onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
    if vs.GuildID != r.GuildID { return }

    userID := vs.UserID
    now := time.Now().UTC()
    isAFK := (vs.ChannelID == r.AfkChannelID)

    r.muVoice.Lock()
    joinedAt, tracked := r.voiceJoin[userID]

    // локальное решение, что делать
    type action int
    const (
        none action = iota
        closeInterval    // закрыть интервал [joinedAt, now]
        startNew         // стартануть новый интервал с now
        dropTracking     // убрать из карты
    )
    var aClose, aStart action

    switch {
    case vs.ChannelID == "":
        if tracked { aClose = closeInterval; aStart = dropTracking }
    case isAFK:
        if tracked { aClose = closeInterval; aStart = dropTracking }
    default:
        if tracked { aClose = closeInterval; aStart = startNew }
        if !tracked { aStart = startNew }
    }
    r.muVoice.Unlock()

    // Закрываем интервал (добавим XP), результат не используем
if aClose == closeInterval && tracked {
    r.addVoiceXPWithCarry(userID, joinedAt, now) // возвраты игнорируем
}

    // Старт/дроп под короткой блокировкой
    r.muVoice.Lock()
    defer r.muVoice.Unlock()
    switch aStart {
    case startNew:
        r.voiceJoin[userID] = now
    case dropTracking:
        delete(r.voiceJoin, userID)
    }
}


func (r *Registry) addVoiceXP(userID string, from, to time.Time) {
	sec := to.Sub(from).Seconds()
	 log.Printf("[level] addVoiceXP user=%s sec=%.1f", userID, sec)
	if sec <= 0 {
		return
	}
	// 100 XP / 3600 сек => 100/3600 XP в сек; берём пол
	xpAdd := int64(math.Floor(sec * (100.0 / 3600.0))) 
	if xpAdd <= 0 {
		return
	}

	ctx := context.Background()
	u, err := UpsertUser(ctx, r.DB, r.GuildID, userID, "", "")
	if err != nil {
		log.Println("[level] voice upsert user err:", err)
		return
	}

	newXP := u.XP + xpAdd
	newLevel := xpToLevel(newXP)
	addSec := int64(sec)

	// addVoiceXP
if err := UpdateAfterVoice(ctx, r.DB, r.GuildID, userID, newXP, newLevel, addSec); err != nil {
    log.Println("[level] UpdateAfterVoice err:", err)
    return
}
// было: if newLevel != u.Level { ... }
if err := r.applyLevelRoles(userID, newLevel); err != nil {
    log.Println("[level] apply roles (voice) err:", err)
}
}

// XP/сек по тарифу (100 XP за час)
const voiceXPPerHour = 100.0
const voiceRate = voiceXPPerHour / 3600.0   // XP в секунду
const secondsPerXP = 3600.0 / voiceXPPerHour // сколько секунд на 1 XP

// addVoiceXPWithCarry начисляет XP за интервал [from, to] и возвращает:
// - newFrom: новый "старт" интервала с сохранением дробного хвоста секунд
// - added:   начислили ли >= 1 XP (если 0 — from не трогаем, чтобы не терять хвост)
func (r *Registry) addVoiceXPWithCarry(userID string, from, to time.Time) (time.Time, bool) {
	sec := to.Sub(from).Seconds()
	if sec <= 0 {
		return from, false
	}

	xpAddFloat := sec * voiceRate
	xpAdd := int64(math.Floor(xpAddFloat))
	if xpAdd <= 0 {
		// XP ещё не «накапал» — ничего не делаем и НЕ сдвигаем from
		return from, false
	}

	ctx := context.Background()
	u, err := UpsertUser(ctx, r.DB, r.GuildID, userID, "", "")
	if err != nil {
		log.Println("[level] voice upsert user err:", err)
		return from, false
	}

	newXP := u.XP + xpAdd
	newLevel := xpToLevel(newXP)

	// добавляем voice-секунды полностью (фактически прошедшие)
	if err := UpdateAfterVoice(ctx, r.DB, r.GuildID, userID, newXP, newLevel, int64(sec)); err != nil {
		log.Println("[level] UpdateAfterVoice err:", err)
		return from, false
	}
	if err := r.applyLevelRoles(userID, newLevel); err != nil {
		log.Println("[level] apply roles (voice) err:", err)
	}

	// «списываем» только секунды, которые дали целые XP, хвост оставляем
	spentSec := float64(xpAdd) * secondsPerXP
	newFrom := from.Add(time.Duration(spentSec * float64(time.Second)))
	return newFrom, true
}


func nickFromMember(m *discordgo.Member) string {
	if m == nil {
		return ""
	}
	if m.Nick != "" {
		return m.Nick
	}
	if m.User != nil {
		return m.User.Username
	}
	return ""
}
