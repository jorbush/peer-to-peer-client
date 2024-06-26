package core

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"gotorrent/bittorrent/network"
	"gotorrent/ui"
	"log"
	"runtime"
	"time"
)

// MaxBlockSize is the largest number of bytes a request can ask for
const MaxBlockSize = 16384

// MaxBacklog is the number of unfulfilled requests a client can have in its pipeline
const (
	InitialBacklog = 5  // Initial number of unfulfilled requests
	MaxBacklog     = 50 // Max number of unfulfilled requests
	MinBacklog     = 1  // Min number of unfulfilled requests
)

var (
	backlog     = InitialBacklog
	backlogStep = 5 // Number of unfulfilled requests to add/subtract
)

type pieceWork struct {
	index  int
	hash   [20]byte
	length int
}

type pieceResult struct {
	index int
	buf   []byte
}

type pieceProgress struct {
	index      int
	client     *Client
	buf        []byte
	downloaded int
	requested  int
	backlog    int
}

func (state *pieceProgress) readMessage() error {
	msg, err := state.client.Read() // this call blocks
	if err != nil {
		return err
	}

	if msg == nil { // keep-alive
		return nil
	}

	switch msg.ID {
	case network.MsgUnchoke:
		state.client.Choked = false
	case network.MsgChoke:
		state.client.Choked = true
	case network.MsgHave:
		index, err := network.ParseHave(msg)
		if err != nil {
			return err
		}
		state.client.Bitfield.SetPiece(index)
	case network.MsgPiece:
		n, err := network.ParsePiece(state.index, state.buf, msg)
		if err != nil {
			return err
		}
		state.downloaded += n
		state.backlog--
	}
	return nil
}

func adjustBacklog(success bool) {
	if success && backlog < MaxBacklog {
		backlog += backlogStep
		log.Printf("Increased backlog to %d\n", backlog)
	} else if !success && backlog > MinBacklog {
		backlog -= backlogStep
		log.Printf("Decreased backlog to %d\n", backlog)
	}
}

func attemptDownloadPiece(c *Client, pw *pieceWork) ([]byte, error) {
	state := pieceProgress{
		index:  pw.index,
		client: c,
		buf:    make([]byte, pw.length),
	}

	// Setting a deadline helps get unresponsive peers unstuck.
	// 30 seconds is more than enough time to download a 262 KB piece
	if err := c.Conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		log.Printf("Failed to set deadline: %v", err)
	}

	defer func() {
		if err := c.Conn.SetDeadline(time.Time{}); err != nil {
			log.Printf("Failed to reset deadline: %v", err)
		}
	}()

	for state.downloaded < pw.length {
		// If unchoked, send requests until we have enough unfulfilled requests
		if !state.client.Choked {
			for state.backlog < backlog && state.requested < pw.length {
				blockSize := MaxBlockSize
				// Last block might be shorter than the typical block
				if pw.length-state.requested < blockSize {
					blockSize = pw.length - state.requested
				}

				err := c.SendRequest(pw.index, state.requested, blockSize)
				if err != nil {
					adjustBacklog(false)
					return nil, err
				}
				state.backlog++
				state.requested += blockSize
			}
		}

		err := state.readMessage()
		if err != nil {
			adjustBacklog(false)
			return nil, err
		}
		adjustBacklog(true)
	}

	return state.buf, nil
}

func checkIntegrity(pw *pieceWork, buf []byte) error {
	hash := sha1.Sum(buf)
	if !bytes.Equal(hash[:], pw.hash[:]) {
		return fmt.Errorf("index %d failed integrity check", pw.index)
	}
	return nil
}

func (t *Torrent) startDownloadWorker(peer network.Peer, workQueue chan *pieceWork, results chan *pieceResult) {
	c, err := NewClient(peer, t.PeerID, t.InfoHash)
	if err != nil {
		log.Printf("Could not handshake with %s. Disconnecting\n", peer.IP)
		log.Println(err)
		return
	}
	defer c.Conn.Close()
	log.Printf("Completed handshake with %s\n", peer.IP)

	if err := c.SendUnchoke(); err != nil {
		log.Printf("Failed to send unchoke: %v", err)
	}

	if err := c.SendInterested(); err != nil {
		log.Printf("Failed to send interested: %v", err)
	}

	for pw := range workQueue {
		if !c.Bitfield.HasPiece(pw.index) {
			workQueue <- pw // Put piece back on the queue
			continue
		}

		// Download the piece
		buf, err := attemptDownloadPiece(c, pw)
		if err != nil {
			log.Println("Exiting", err)
			workQueue <- pw // Put piece back on the queue
			return
		}

		err = checkIntegrity(pw, buf)
		if err != nil {
			log.Printf("Piece #%d failed integrity check\n", pw.index)
			workQueue <- pw // Put piece back on the queue
			continue
		}

		if err := c.SendHave(pw.index); err != nil {
			log.Printf("Failed to send have: %v", err)
		}
		results <- &pieceResult{pw.index, buf}
	}
}

func (t *Torrent) calculateBoundsForPiece(index int) (begin int, end int) {
	begin = index * t.PieceLength
	end = begin + t.PieceLength
	if end > t.Length {
		end = t.Length
	}
	return begin, end
}

func (t *Torrent) calculatePieceSize(index int) int {
	begin, end := t.calculateBoundsForPiece(index)
	return end - begin
}

// Download downloads the torrent. This stores the entire file in memory.
func (t *Torrent) Download() ([]byte, error) {
	log.Println("Starting download for", t.Name)
	fmt.Printf("Starting download for \033[36m%s\033[0m...\n", t.Name)
	// Init queues for workers to retrieve work and send results
	workQueue := make(chan *pieceWork, len(t.PieceHashes))
	results := make(chan *pieceResult)

	// Initialize the progress bar
	pb := ui.NewPBar()
	pb.SignalHandler()
	pb.Total = uint16(100)

	for index, hash := range t.PieceHashes {
		length := t.calculatePieceSize(index)
		workQueue <- &pieceWork{index, hash, length}
	}

	// Start workers
	for _, peer := range t.Peers {
		go t.startDownloadWorker(peer, workQueue, results)
	}

	// Collect results into a buffer until full
	buf := make([]byte, t.Length)
	donePieces := 0
	for donePieces < len(t.PieceHashes) {
		res := <-results
		begin, end := t.calculateBoundsForPiece(res.index)
		copy(buf[begin:end], res.buf)
		donePieces++

		percent := float64(donePieces) / float64(len(t.PieceHashes)) * 100 // Convert percent to float64
		numWorkers := runtime.NumGoroutine() - 1                           // subtract 1 for main thread

		// Save into a logs file
		log.Printf("(%0.2f%%) Downloaded piece #%d from %d peers\n", percent, res.index, numWorkers)
		pb.RenderPBar(percent, res.index, numWorkers)
	}
	close(workQueue)
	pb.CleanUp()
	fmt.Printf("\n\033[32mFile %s downloaded!\033[0m\n", t.Name)
	fmt.Println("Check the output directory.")
	return buf, nil
}
