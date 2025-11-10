package level

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type UserRow struct {
	GuildID     string
	UserID      string
	Username    string
	DisplayName string
	XP          int64
	Level       int
	LastMsgAt   *time.Time
	VoiceSec    int64
}

func UpsertUser(ctx context.Context, db *pgxpool.Pool, guildID, userID, username, display string) (*UserRow, error) {
	_, err := db.Exec(ctx, `
INSERT INTO users_levels (guild_id, user_id, username, display_name)
VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''))
ON CONFLICT (guild_id, user_id) DO UPDATE
SET
  username     = COALESCE(NULLIF($3,''), users_levels.username),
  display_name = COALESCE(NULLIF($4,''), users_levels.display_name),
  updated_at   = now()
`, guildID, userID, username, display)
	if err != nil {
		return nil, err
	}
	return GetUser(ctx, db, guildID, userID)
}

func GetUser(ctx context.Context, db *pgxpool.Pool, guildID, userID string) (*UserRow, error) {
	var u UserRow
	err := db.QueryRow(ctx, `
SELECT guild_id, user_id, COALESCE(username,''), COALESCE(display_name,''),
       xp, level, last_msg_at, voice_sec_accum
FROM users_levels
WHERE guild_id=$1 AND user_id=$2
`, guildID, userID).Scan(
		&u.GuildID, &u.UserID, &u.Username, &u.DisplayName,
		&u.XP, &u.Level, &u.LastMsgAt, &u.VoiceSec,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func UpdateAfterMessage(ctx context.Context, db *pgxpool.Pool, guildID, userID string, newXP int64, now *time.Time, newLevel int) error {
	_, err := db.Exec(ctx, `
UPDATE users_levels
SET xp=$1, level=$2, last_msg_at=$3, updated_at=now()
WHERE guild_id=$4 AND user_id=$5
`, newXP, newLevel, now, guildID, userID)
	return err
}

func UpdateAfterVoice(ctx context.Context, db *pgxpool.Pool, guildID, userID string, newXP int64, newLevel int, addSec int64) error {
	_, err := db.Exec(ctx, `
UPDATE users_levels
SET xp=$1, level=$2, voice_sec_accum = voice_sec_accum + $3, updated_at=now()
WHERE guild_id=$4 AND user_id=$5
`, newXP, newLevel, addSec, guildID, userID)
	return err
}
