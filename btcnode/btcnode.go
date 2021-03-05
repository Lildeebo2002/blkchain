package btcnode

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/blkchain/blkchain"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
)

type btcNode struct {
	*peer.Peer
	tmout     time.Duration
	headersCh chan []*wire.BlockHeader
	blockCh   chan *wire.MsgBlock
	invCh     chan *wire.MsgInv

	height   int // current height
	n        int // pos within height
	count    int
	byHeight map[int][]*blkchain.BlockHeader
}

type heightBH struct {
	height int
	bh     *blkchain.BlockHeader
}

func (b *btcNode) Count() int {
	return b.count
}

func (b *btcNode) CurrentHeight() int {
	return b.height
}

func (b *btcNode) Next() bool {
	if len(b.byHeight) == 0 {
		return false
	}
	if b.n < len(b.byHeight[b.height])-1 {
		b.n++
	} else {
		if len(b.byHeight[b.height+1]) == 0 {
			return false
		}
		b.height++
		b.n = 0
	}
	return true
}

func (b *btcNode) BlockHeader() *blkchain.BlockHeader {
	if len(b.byHeight[b.height]) > 0 {
		return b.byHeight[b.height][b.n]
	}
	return nil
}

func (b *btcNode) ReadBlock() (*blkchain.Block, error) {
	bh := b.BlockHeader()
	return b.getBlock(bh.Hash())
}

func (b *btcNode) getHeaders(startHashes map[int][]blkchain.Uint256) error {

	b.headersCh = make(chan []*wire.BlockHeader)

	byPrevHash := make(map[blkchain.Uint256][]*heightBH, 2000)

	// initial lastHashes, must be in height reverse order as
	// (contrary to docs) it seems to start at the first recognized
	// blockhash, not the last one
	lastHashes := make([]blkchain.Uint256, 0, len(startHashes))
	var minHeight int
	for h, _ := range startHashes {
		if minHeight == 0 || minHeight > h {
			minHeight = h
		}
	}
	for i := len(startHashes) - 1; i >= 0; i-- {
		for _, hash := range startHashes[minHeight+i] {
			lastHashes = append(lastHashes, hash)
		}
	}

	for {

		bLocator := make(blockchain.BlockLocator, len(lastHashes))
		for i, hash := range lastHashes {
			// This avoids a subtle issue/bug? whereby we pass a
			// pointer to an array which we later modify, and since it's
			// a pointer it gets modified where we passed it to as well.
			hCopy := chainhash.Hash(hash)
			bLocator[i] = &hCopy
		}
		b.PushGetHeadersMsg(bLocator, &chainhash.Hash{})

		var hdrs []*wire.BlockHeader
		select {
		case hdrs = <-b.headersCh:
		case <-time.After(b.tmout):
			return fmt.Errorf("Time out.")
		}

		if len(hdrs) == 0 { // No more headers
			log.Printf("End of headers (for now).")
			break
		} else {
			log.Printf("Received batch of %d headers.", len(hdrs))
		}

		for _, h := range hdrs {
			bh := &blkchain.BlockHeader{
				Version:        blkchain.Uint32(h.Version),
				PrevHash:       blkchain.Uint256(h.PrevBlock),
				HashMerkleRoot: blkchain.Uint256(h.MerkleRoot),
				Time:           blkchain.Uint32(h.Timestamp.Unix()),
				Bits:           blkchain.Uint32(h.Bits),
				Nonce:          blkchain.Uint32(h.Nonce),
			}
			if list, ok := byPrevHash[bh.PrevHash]; !ok {
				byPrevHash[bh.PrevHash] = []*heightBH{&heightBH{0, bh}}
			} else {
				byPrevHash[bh.PrevHash] = append(list, &heightBH{0, bh})
			}
			b.count++

			lastHashes = []blkchain.Uint256{bh.Hash()}
		}
	}

	if len(byPrevHash) == 0 { // Nothing to do
		return nil
	}

	// Assign heights
	foundHeight := setChildrenHeight(byPrevHash, startHashes)
	b.height = foundHeight // this is current height - 1

	if b.byHeight == nil {
		b.byHeight = make(map[int][]*blkchain.BlockHeader, len(byPrevHash))
	}

	for _, hbhs := range byPrevHash {
		for _, hbh := range hbhs {
			if list, ok := b.byHeight[hbh.height]; !ok {
				b.byHeight[hbh.height] = []*blkchain.BlockHeader{hbh.bh}
			} else {
				b.byHeight[hbh.height] = append(list, hbh.bh)
			}
		}
	}

	count, err := eliminateOrphans(b.byHeight)
	if err != nil {
		return err
	}
	b.count = count

	return nil

}

func txFromMsgTx(mtx *wire.MsgTx) *blkchain.Tx {
	tx := &blkchain.Tx{
		Version:  uint32(mtx.Version),
		TxIns:    make(blkchain.TxInList, 0, len(mtx.TxIn)),
		TxOuts:   make(blkchain.TxOutList, 0, len(mtx.TxOut)),
		LockTime: uint32(mtx.LockTime),
		SegWit:   false,
	}
	// TxIns
	for _, in := range mtx.TxIn {
		txin := &blkchain.TxIn{
			PrevOut: blkchain.OutPoint{
				Hash: blkchain.Uint256(in.PreviousOutPoint.Hash),
				N:    in.PreviousOutPoint.Index,
			},
			ScriptSig: in.SignatureScript,
			Sequence:  in.Sequence,
			Witness:   make(blkchain.Witness, 0, len(in.Witness)),
		}
		for _, w := range in.Witness {
			txin.Witness = append(txin.Witness, w)
		}
		if !tx.SegWit && len(txin.Witness) > 0 {
			tx.SegWit = true
		}
		tx.TxIns = append(tx.TxIns, txin)
	}
	// TxOuts
	for _, out := range mtx.TxOut {
		tx.TxOuts = append(tx.TxOuts, &blkchain.TxOut{
			Value:        out.Value,
			ScriptPubKey: out.PkScript,
		})
	}
	return tx
}

func blockFromMsgBlock(mb *wire.MsgBlock, magic uint32) *blkchain.Block {
	blk := &blkchain.Block{
		Magic: magic,
		BlockHeader: &blkchain.BlockHeader{
			Version:        blkchain.Uint32(mb.Header.Version),
			PrevHash:       blkchain.Uint256(mb.Header.PrevBlock),
			HashMerkleRoot: blkchain.Uint256(mb.Header.MerkleRoot),
			Time:           blkchain.Uint32(mb.Header.Timestamp.Unix()),
			Bits:           blkchain.Uint32(mb.Header.Bits),
			Nonce:          blkchain.Uint32(mb.Header.Nonce),
		},
		Txs: make(blkchain.TxList, 0, len(mb.Transactions)),
	}
	for _, mtx := range mb.Transactions {
		blk.Txs = append(blk.Txs, txFromMsgTx(mtx))
	}
	return blk
}

// Get a block from the node
func (b *btcNode) getBlock(hash blkchain.Uint256) (*blkchain.Block, error) {

	if b.blockCh == nil {
		b.blockCh = make(chan *wire.MsgBlock)
	}

	gdmsg := wire.NewMsgGetData()
	gdmsg.AddInvVect(wire.NewInvVect(wire.InvTypeWitnessBlock, (*chainhash.Hash)(&hash)))
	b.QueueMessage(gdmsg, nil)

	var block *wire.MsgBlock
	select {
	case block = <-b.blockCh:
	case <-time.After(b.tmout):
		return nil, fmt.Errorf("Time out.")
	}

	return blockFromMsgBlock(block, blkchain.MainNetMagic), nil
}

// Recursively (from lowest height) assign height. Return the
// satisfied height from the given parentHashes (assuming all is well,
// there should only be one because of how the nodes respond to
// getheaders)
func setChildrenHeight(byPrevHash map[blkchain.Uint256][]*heightBH, parentHashes map[int][]blkchain.Uint256) (foundHeight int) {
	for parentHeight, parents := range parentHashes {
		for _, parentHash := range parents {
			for _, child := range byPrevHash[parentHash] {
				child.height = parentHeight + 1
				foundHeight = parentHeight // this height found children in the node response
				// log.Printf("%v %v", child.height, child.bh.Hash())
				setChildrenHeight(byPrevHash, map[int][]blkchain.Uint256{child.height: []blkchain.Uint256{child.bh.Hash()}})
			}
		}
	}
	return foundHeight
}

func ReadBtcnodeBlockHeaderIndex(addr string, tmout time.Duration, hashes map[int][]blkchain.Uint256) (blkchain.BlockHeaderIndex, error) {

	node, err := ConnectToNode(addr, tmout)
	if err != nil {
		return nil, err
	}

	// Get headers
	if err := node.getHeaders(hashes); err != nil {
		return nil, err
	}

	return node, nil
}

func ConnectToNode(addr string, tmout time.Duration) (*btcNode, error) {

	result := &btcNode{
		tmout: tmout,
	}

	verackCh := make(chan bool)
	peerCfg := &peer.Config{
		DisableRelayTx:   true,
		UserAgentName:    "blkchain", // User agent name to advertise.
		UserAgentVersion: "0.0.1",    // User agent version to advertise.
		ChainParams:      &chaincfg.MainNetParams,
		Services:         0,
		TrickleInterval:  time.Second * 10,
		Listeners: peer.MessageListeners{
			OnVerAck: func(p *peer.Peer, msg *wire.MsgVerAck) {
				verackCh <- true
			},
			OnBlock: func(_ *peer.Peer, msg *wire.MsgBlock, buf []byte) {
				if result.blockCh != nil {
					result.blockCh <- msg
				}
			},
			OnHeaders: func(p *peer.Peer, msg *wire.MsgHeaders) {
				if result.headersCh != nil {
					result.headersCh <- msg.Headers
				}
			},
			OnInv: func(p *peer.Peer, msg *wire.MsgInv) {
				if result.invCh != nil {
					result.invCh <- msg
				}
			},
		},
	}

	p, err := peer.NewOutboundPeer(peerCfg, addr)
	if err != nil {
		return nil, err
	}

	// Establish the connection to the peer address and mark it connected.
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		return nil, err
	}

	p.AssociateConnection(conn)

	select {
	case <-verackCh:
		// Verack pretty much means we are connected
	case <-time.After(tmout):
		p.Disconnect()
		return nil, fmt.Errorf("Connection timeout")
	}
	result.Peer = p

	return result, nil
}

func (b *btcNode) Close() error {
	b.Disconnect()
	return nil
}

// Wait for inventory of block(s) from the node.
func (b *btcNode) WaitForBlock(interrupt chan bool) ([]*blkchain.Block, error) {

	if interrupt == nil {
		interrupt = make(chan bool)
	}

	if b.invCh == nil {
		b.invCh = make(chan *wire.MsgInv)
	}

	for {

		var msg *wire.MsgInv
		select {
		case msg = <-b.invCh:
		case <-interrupt:
			interrupt <- true // to keep len() > 0
			return nil, fmt.Errorf("Interrupted.")
		}

		if msg == nil {
			return nil, fmt.Errorf("Received nil message?")
		}

		result := make([]*blkchain.Block, 0)
		for _, inv := range msg.InvList {

			hash := blkchain.Uint256(inv.Hash)

			if inv.Type == wire.InvTypeBlock || inv.Type == wire.InvTypeWitnessBlock {
				// NB: Seems like it's always InvTypeBlock, never InvTypeWitnessBlock.

				blk, err := b.getBlock(hash)

				if err != nil {
					return nil, err
				}

				result = append(result, blk)
			} else {
				log.Printf("WARNING: Unknown inv.Type %v for hash %v", inv.Type, hash)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}

	return nil, nil
}

// Eliminate orphans by walking the chan backwards and whenever we
// see more than one block at a height, picking the one that
// matches its descendant's PrevHash.
func eliminateOrphans(m map[int][]*blkchain.BlockHeader) (int, error) {

	minHeight, maxHeight, count := -1, -1, 0

	// Find min, max and count
	for h, v := range m {
		if minHeight > h || minHeight == -1 {
			minHeight = h
		}
		if maxHeight < h || maxHeight == -1 {
			maxHeight = h
		}
		count += len(v)
	}

	// It is possible that we are at a split, i.e. more than block
	// exists at max height.  we can just delete them until the main
	// chain unity is found. TODO: We can do better.
	for h := maxHeight; len(m[h]) > 1 && h >= minHeight; h-- {
		log.Printf("Chain is split at heighest height, ignoring height %d", h)
		delete(m, h)
		maxHeight--
	}

	prevHash := m[maxHeight][0].PrevHash
	for h := maxHeight - 1; h >= minHeight; h-- {
		if len(m[h]) > 1 { // More than one block at this height
			for _, bh := range m[h] {
				if bh.Hash() == prevHash {
					m[h] = []*blkchain.BlockHeader{bh}
				} else {
					log.Printf("Ignoring orphan block %v", bh.Hash())
					count--
				}
			}
			if len(m[h]) != 1 {
				return count, fmt.Errorf("Problem finding valid parent when eliminating orphans.")
			}
		}

		if len(m[h]) > 0 {
			prevHash = m[h][0].PrevHash
		}
	}

	return count, nil
}
