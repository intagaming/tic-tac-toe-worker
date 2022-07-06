package worker

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/ably/ably-go/ably"
)

type QueueMessage struct {
	Source  string `json:"source"`
	AppId   string `json:"appId"`
	Channel string `json:"channel"`
	Site    string `json:"site"`
	RuleId  string `json:"ruleId"`
}

type PresenceMessage struct {
	*QueueMessage
	Presence []Presence `json:"presence"`
}

type MessageMessage struct {
	*QueueMessage
	Messages []Message `json:"messages"`
}

type Presence struct {
	Id           string `json:"id"`
	ClientId     string `json:"clientId"`
	ConnectionId string `json:"connectionId"`
	Timestamp    int    `json:"timestamp"`
	Name         string `json:"name"`
	Action       int    `json:"action"`
	Data         string `json:"data"`
}

type Message struct {
	Id           string `json:"id"`
	ClientId     string `json:"clientId"`
	ConnectionId string `json:"connectionId"`
	Timestamp    int    `json:"timestamp"`
	Name         string `json:"name"`
	Data         string `json:"data"`
}

func unmarshalPresence(payload []byte) (*PresenceMessage, error) {
	msg := &PresenceMessage{}
	err := json.Unmarshal(payload, msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func unmarshalMessage(payload []byte) (*MessageMessage, error) {
	msg := &MessageMessage{}
	err := json.Unmarshal(payload, msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func handle(ctx context.Context, payload []byte) {
	payloadString := string(payload)
	if strings.Contains(payloadString, "channel.presence") {
		msg, err := unmarshalPresence(payload)
		if err != nil {
			log.Println("Error unmarshalling presence message: ", err)
			return
		}
		handlePresence(ctx, msg)
	} else if strings.Contains(payloadString, "channel.message") {
		msg, err := unmarshalMessage(payload)
		if err != nil {
			log.Println("Error unmarshalling message: ", err)
			return
		}
		handleMessage(ctx, msg)
	} else {
		log.Println("Unknown message: ", payloadString)
	}
}

func handlePresence(ctx context.Context, presenceMsg *PresenceMessage) {
	msg := presenceMsg.Presence[0]
	switch msg.Action {
	case int(ably.PresenceActionEnter):
		onEnter(ctx, presenceMsg)
	case int(ably.PresenceActionLeave):
		onLeave(ctx, presenceMsg)
	}
}

func handleMessage(ctx context.Context, messageMsg *MessageMessage) {
	onMessage(ctx, messageMsg)
}
