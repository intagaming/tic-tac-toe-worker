package worker

import (
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
	Presence []Message `json:"presence"`
}

type MessageMessage struct {
	*QueueMessage
	Messages []Message `json:"messages"`
}

type Message struct {
	Id           string `json:"id"`
	ClientId     string `json:"clientId"`
	ConnectionId string `json:"connectionId"`
	Timestamp    int    `json:"timestamp"`
	Name         string `json:"name"`
	Action       int    `json:"action"`
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

func handle(payload []byte) {
	json := string(payload)
	if strings.Contains(json, "channel.presence") {
		msg, err := unmarshalPresence(payload)
		if err != nil {
			log.Println("Error unmarshalling presence message: ", err)
			return
		}
		handlePresence(msg)
	} else if strings.Contains(json, "channel.message") {
		msg, err := unmarshalMessage(payload)
		if err != nil {
			log.Println("Error unmarshalling message: ", err)
			return
		}
		handleMessage(msg)
	} else {
		log.Println("Unknown message: ", json)
	}
}

func handlePresence(presenceMsg *PresenceMessage) {
	msg := presenceMsg.Presence[0]
	// log.Println("Handling presence: ", msg)
	switch msg.Action {
	case int(ably.PresenceActionEnter):
		log.Printf("%s entered channel %s\n", msg.ClientId, presenceMsg.Channel)
	case int(ably.PresenceActionLeave):
		log.Printf("%s left channel %s\n", msg.ClientId, presenceMsg.Channel)
	}
}

func handleMessage(messageMsg *MessageMessage) {
	msg := messageMsg.Messages[0]
	log.Println("Handling message: ", msg)
}
