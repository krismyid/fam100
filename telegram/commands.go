package main

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/uber-go/zap"
	"github.com/yulrizka/bot"
	"github.com/yulrizka/fam100"
)

var (
	// cmdRateDelay is time before we serve score command
	cmdRateDelay = 30 * time.Second

	gameQueue chan struct{}
)

var lastCmdRequest = make(map[string]time.Time)

// handleJoin handles "/join". Create game and start it if quorum
func (b *fam100Bot) cmdJoin(msg *bot.Message) bool {
	defer cmdJoinTimer.UpdateSince(time.Now())

	if b.handleDisabled(msg) {
		return true
	}

	commandJoinCount.Inc(1)
	chanID := msg.Chat.ID
	chanName := msg.Chat.Title
	ch, ok := b.channels[chanID]
	if !ok {
		playerJoinedCount.Inc(1)
		// create a new game
		quorumPlayer := map[string]bool{msg.From.ID: true}
		players := map[string]string{msg.From.ID: msg.From.FullName()}

		gameIn := make(chan fam100.Message, gameInBufferSize)
		game, err := fam100.NewGame(chanID, chanName, gameIn, b.gameOut)
		if err != nil {
			log.Error("creating a game", zap.String("chanID", chanID))
			return true
		}

		ch := &channel{ID: chanID, game: game, quorumPlayer: quorumPlayer, players: players}
		b.channels[chanID] = ch
		if len(ch.quorumPlayer) == minQuorum {
			ch.game.Start()
			return true
		}
		log.Info("User joined", zap.String("playerID", msg.From.ID), zap.String("chanID", chanID))

		go func() {
			start := time.Now()
			// handle queueing
			shouldBeQueued := true
			if queueConf, err := fam100.DefaultDB.ChannelConfig(chanID, "queued", ""); err == nil && queueConf == "no" {
				shouldBeQueued = false
			}

			select {
			case gameQueue <- struct{}{}:
				game.State = fam100.Created
			default:
				if !shouldBeQueued {
					game.State = fam100.Created
					break
				}

				// it's waiting for the queue
				text := fmt.Sprintf(
					"game telah dimasukkan dalam antrian, rata-rata waktu antrian <b>%.0fs</b>. User lain dapat tetap melakukan '/join@%s'. Game akan dimulai automatis ketika antrian selesai",
					time.Duration(gameWaitingTimer.Mean()).Seconds(),
					botName)

				b.out <- bot.Message{Chat: bot.Chat{ID: chanID}, Text: text, Format: bot.HTML, DiscardAfter: time.Now().Add(5 * time.Second)}
				log.Info("game in queue", zap.Int64("gameID", game.ID), zap.String("chanID", chanID))

				gameCurrentQueuedCount.Inc(1)
				gameQueue <- struct{}{} // wait until it's available
				gameCurrentQueuedCount.Dec(1)
				gameWaitingTimer.UpdateSince(start)
				game.State = fam100.Created

				// maybe it's already quorum after finished waiting
				if len(ch.quorumPlayer) >= minQuorum {
					ch.game.Start()
					return
				}
			}
			gameCurrentQuorumCount.Inc(1)
			ch.startQuorumTimer(time.Duration(quorumWait)*time.Second, b.out)
			ch.startQuorumNotifyTimer(5*time.Second, b.out)
		}()
		return true
	}

	switch {
	case ch.game.State == fam100.Queued:
	case ch.game.State == fam100.Created:
	case ch.quorumPlayer[msg.From.ID]:
		return true
	default:
		return true
	}

	// new player joined
	playerJoinedCount.Inc(1)
	ch.quorumPlayer[msg.From.ID] = true
	ch.players[msg.From.ID] = msg.From.FullName()

	if ch.game.State == fam100.Queued {
		return true
	}

	// state is Created, waiting for quorum
	ch.cancelTimer()
	if len(ch.quorumPlayer) >= minQuorum {
		if ch.cancelNotifyTimer != nil {
			ch.cancelNotifyTimer()
		}

		gameCurrentQuorumCount.Dec(1)
		ch.game.Start()
		return true
	}
	ch.startQuorumTimer(time.Duration(quorumWait)*time.Second, b.out)
	if ch.cancelNotifyTimer == nil {
		ch.startQuorumNotifyTimer(5*time.Second, b.out)
	}
	log.Info("User joined", zap.String("playerID", msg.From.ID), zap.String("chanID", chanID))

	return true
}

func (b *fam100Bot) cmdHelp(msg *bot.Message) bool {
	defer cmdHelpTimer.UpdateSince(time.Now())

	if b.handleDisabled(msg) {
		return true
	}

	if rateLimited("help", msg.Chat.ID, cmdRateDelay) {
		return true
	}
	text := `Cara bermain, menambahkan bot ke group sendiri dapat dilihat di <a href="http://labs.yulrizka.com/fam100/faq.html">F.A.Q</a>`
	b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: text, Format: bot.HTML, DiscardAfter: time.Now().Add(5 * time.Second)}

	return true
}

// handleJoin handles "/score" show top score for current channel
func (b *fam100Bot) cmdScore(msg *bot.Message) bool {
	defer cmdScoreTimer.UpdateSince(time.Now())

	if b.handleDisabled(msg) {
		return true
	}

	if rateLimited("score", msg.Chat.ID, cmdRateDelay) {
		return true
	}

	go func() {
		commandScoreCount.Inc(1)
		chanID := msg.Chat.ID
		rank, err := fam100.DefaultDB.ChannelRanking(chanID, 20)
		if err != nil {
			log.Error("getting channel ranking failed", zap.String("chanID", chanID), zap.Error(err))
			return
		}

		text := "<b>Top Score:</b>\n" + formatRankText(rank)
		text += fmt.Sprintf("\n<a href=\"http://labs.yulrizka.com/fam100/scores.html?c=%s\">Full Score</a>", chanID)
		b.out <- bot.Message{Chat: bot.Chat{ID: chanID}, Text: text, Format: bot.HTML, DiscardAfter: time.Now().Add(20 * time.Second)}
	}()

	return true
}

func (b *fam100Bot) handleDisabled(msg *bot.Message) bool {
	chanID := msg.Chat.ID
	disabledMsg, _ := fam100.DefaultDB.ChannelConfig(chanID, "disabled", "")

	if disabledMsg != "" {
		log.Debug("channel is disabled", zap.String("chanID", chanID), zap.String("msg", disabledMsg))
		b.out <- bot.Message{Chat: bot.Chat{ID: chanID}, Text: disabledMsg, Format: bot.Markdown, DiscardAfter: time.Now().Add(5 * time.Second)}
		return true
	}

	return false
}

func formatRoundText(msg fam100.QNAMessage) string {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)

	fmt.Fprintf(w, "[id: %d] %s?\n\n", msg.QuestionID, msg.QuestionText)
	for i, a := range msg.Answers {
		if a.Answered {
			if a.Highlight {
				fmt.Fprintf(w, "<b>%d. (%2d) %s \n  ✓ %s</b>\n", i+1, a.Score, escape(a.Text), escape(a.PlayerName))
			} else {
				fmt.Fprintf(w, "%d. (%2d) %s \n  ✓ <i>%s</i>\n", i+1, a.Score, escape(a.Text), escape(a.PlayerName))
			}
		} else {
			if msg.ShowUnanswered {
				fmt.Fprintf(w, "<b>%d. (%2d) %s \n</b>", i+1, a.Score, escape(a.Text))
			} else {
				fmt.Fprintf(w, "%d. _________________________\n", i+1)
			}
		}
	}
	w.Flush()

	return b.String()
}

func formatRankText(rank fam100.Rank) string {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)

	fmt.Fprintf(w, "\n")
	lastPos := 0
	if len(rank) == 0 {
		fmt.Fprintf(w, fam100.T("Tidak ada\n"))
	} else {
		for _, ps := range rank {
			if lastPos != 0 && lastPos+1 != ps.Position {
				fmt.Fprintf(w, "...\n")
			}
			fmt.Fprintf(w, "%d. (%2d) %s\n", ps.Position, ps.Score, ps.Name)
			lastPos = ps.Position
		}
	}
	w.Flush()

	return escape(b.String())
}

func escape(s string) string {
	s = strings.Replace(s, "&", "&amp;", -1)
	s = strings.Replace(s, "<", "&lt;", -1)
	s = strings.Replace(s, ">", "&gt;", -1)

	return s
}

// cmdSay handles /say [chan_id] [message]
func (b *fam100Bot) cmdSay(msg *bot.Message) bool {
	fields := strings.SplitN(msg.Text, " ", 3)
	if len(fields) < 3 {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "usage: `/say [chanID] [message]`", Format: bot.Markdown}
		return true
	}
	chatID, text := fields[1], fields[2]
	b.out <- bot.Message{Chat: bot.Chat{ID: chatID}, Text: text, Format: bot.Markdown}

	return true
}

// cmdSay handles /channels [pattern]. empty pattern matches all
func (b *fam100Bot) cmdChannels(msg *bot.Message) bool {
	fields := strings.SplitN(msg.Text, " ", 2)
	if len(fields) < 2 || fields[1] == "" {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "usage: `/channels [regex pattern]`", Format: bot.Markdown}
		return true
	}

	channels, err := fam100.DefaultDB.Channels()
	if err != nil {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "channels failed. " + err.Error(), Format: bot.Markdown}
	}

	// filter out by regex
	r, err := regexp.Compile(fields[1])
	if err != nil {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "regex failed. " + err.Error(), Format: bot.Markdown}
	}

	results := make(map[string]string)
	for id, desc := range channels {
		if r.MatchString(desc) {
			results[id] = desc
		}
	}

	buf := bytes.NewBuffer(nil)
	for id, desc := range results {
		if r.MatchString(desc) {
			buf.WriteString("\n")
			buf.WriteString(id)
			buf.WriteString(" ")
			buf.WriteString(desc)
		}
	}

	text := fmt.Sprintf("found %d channels:", len(results))
	body := buf.String()
	if len(body) > 3000 {
		body = body[:3000]
		body = body[:strings.LastIndex(body, "\n")]
		body += "\n ... truncated"
	}

	b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: text + body, Format: bot.Text}

	return true
}

// cmdBroadcast handles /broadcast [msg]. Broadcast message to all channels
func (b *fam100Bot) cmdBroadcast(msg *bot.Message) bool {
	fields := strings.SplitN(msg.Text, " ", 2)
	if len(fields) < 2 || fields[1] == "" {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "usage: `/broadcast [message]`", Format: bot.Markdown}
		return true
	}

	channels, err := fam100.DefaultDB.Channels()
	if err != nil {
		b.out <- bot.Message{Chat: bot.Chat{ID: msg.Chat.ID}, Text: "channels failed. " + err.Error(), Format: bot.Markdown}
	}

	go func() {
		text := fields[1]
		for id := range channels {
			b.out <- bot.Message{Chat: bot.Chat{ID: id}, Text: text, Format: bot.Text}
			time.Sleep(1 * time.Second)
		}
	}()

	return true
}

// rateLimited returns true if call should be ignored becasue of the rate limit
func rateLimited(cmd, chatID string, duration time.Duration) bool {

	now := time.Now()
	if lastTime, ok := lastCmdRequest[chatID]; ok &&
		now.Before(lastTime.Add(duration)) {
		return true
	}

	lastCmdRequest[chatID] = now
	return false
}
