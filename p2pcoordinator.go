package main

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// Data related to the (single instance of) the global p2p coordinator. This is also a
// single-threaded object, its fields and methods are only expected to be accessed from
// the Run() goroutine.
type p2pCoordinatorType struct {
	timeTicks                chan int
	lastTickBlockchainHeight int
	recentlyRequestedBlocks  *StringSetWithExpiry
	lastReconnectTime        time.Time
	badPeers                 *StringSetWithExpiry
}

// XXX: singletons in go?
var p2pCoordinator = p2pCoordinatorType{
	recentlyRequestedBlocks: NewStringSetWithExpiry(5 * time.Second),
	lastReconnectTime:       time.Now(),
	timeTicks:               make(chan int),
	badPeers:                NewStringSetWithExpiry(15 * time.Minute),
}

func (co *p2pCoordinatorType) Run() {
	co.lastTickBlockchainHeight = dbGetBlockchainHeight()
	go co.timeTickSource()
	for {
		select {
		case msg := <-p2pCtrlChannel:
			switch msg.msgType {
			case p2pCtrlSearchForBlocks:
				co.handleSearchForBlocks(msg.payload.(*p2pConnection))
			case p2pCtrlDiscoverPeers:
				co.handleDiscoverPeers(msg.payload.([]string))
			}
		case <-co.timeTicks:
			co.handleTimeTick()
		}
	}
}

func (co *p2pCoordinatorType) timeTickSource() {
	for {
		time.Sleep(1 * time.Second)
		co.timeTicks <- 1
	}
}

// Retrieves block hashes from a node which apparently has more blocks than we do.
// ToDo: This is a simplistic version. Make it better by introducing quorums.
func (co *p2pCoordinatorType) handleSearchForBlocks(p2pcStart *p2pConnection) {
	msg := p2pMsgGetBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgGetBlockHashes,
		},
		MinBlockHeight: dbGetBlockchainHeight(),
		MaxBlockHeight: p2pcStart.chainHeight,
	}
	log.Printf("Searching for blocks from %d to %d", msg.MinBlockHeight, msg.MaxBlockHeight)
	p2pcStart.chanToPeer <- msg
}

func (co *p2pCoordinatorType) handleDiscoverPeers(addresses []string) {
	for _, address := range addresses {
		i := strings.LastIndex(address, ":")
		var host string
		if i > -1 {
			host = address[0:i]
		} else {
			host = address
		}
		canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
		if p2pPeers.HasAddress(canonicalAddress) || co.badPeers.Has(canonicalAddress) {
			continue
		}
		addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
		if err != nil {
			return
		}
		// Detect if there's a canonical peer on the other side, somewhat brute-forceish
		conn, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			return
		}
		p2pc := p2pConnection{
			conn:         conn,
			address:      canonicalAddress,
			chanToPeer:   make(chan interface{}, 5),
			chanFromPeer: make(chan StrIfMap, 5),
		}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
		log.Println("Detected canonical peer at", canonicalAddress)
		dbSavePeer(canonicalAddress)
	}
}

// Executed periodically to perform time-dependant actions. Do not rely on the
// time period to be predictable or precise.
func (co *p2pCoordinatorType) handleTimeTick() {
	newHeight := dbGetBlockchainHeight()
	if newHeight > co.lastTickBlockchainHeight {
		co.floodPeersWithNewBlocks(co.lastTickBlockchainHeight, newHeight)
		co.lastTickBlockchainHeight = newHeight
	}
	if time.Since(co.lastReconnectTime) >= 10*time.Minute {
		co.lastReconnectTime = time.Now()
		co.connectDbPeers()
	}
}

func (co *p2pCoordinatorType) floodPeersWithNewBlocks(minHeight, maxHeight int) {
	blockHashes := dbGetHeightHashes(minHeight, maxHeight)
	msg := p2pMsgBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgBlockHashes,
		},
		Hashes: blockHashes,
	}
	p2pPeers.lock.With(func() {
		for p2pc := range p2pPeers.peers {
			p2pc.chanToPeer <- msg
		}
	})
}

func (co *p2pCoordinatorType) connectDbPeers() {
	peers := dbGetSavedPeers()
	for peer := range peers {
		if p2pPeers.HasAddress(peer) {
			continue
		}
		if co.badPeers.Has(peer) {
			continue
		}
		conn, err := net.Dial("tcp", peer)
		if err != nil {
			log.Println("Error connecting to", peer, err)
			continue
		}
		p2pc := p2pConnection{
			conn:         conn,
			address:      peer,
			chanToPeer:   make(chan interface{}, 5),
			chanFromPeer: make(chan StrIfMap, 5),
		}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
	}
}
