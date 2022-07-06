package shared

type Room struct {
	Id    string        `json:"id"`
	Host  *string       `json:"host"`
	State string        `json:"state"`
	Guest *string       `json:"guest"`
	Data  TicTacToeData `json:"data"`
}

type TicTacToeData struct {
	Ticks      int       `json:"ticks"`
	Board      []*string `json:"board"`
	Turn       string    `json:"turn"`
	TurnEndsAt int       `json:"turnEndsAt"`

	// GameEndsAt is in Unix seconds.
	GameEndsAt int `json:"gameEndsAt"`
}
