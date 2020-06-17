package main

import (
	"flag"
	"fmt"
	"math"

	"github.com/coreos/etcd/raft/raftpb"
	"github.com/thesues/aspira/raftwal"
	"github.com/thesues/aspira/utils"
	"github.com/thesues/cannyls-go/lump"
	"github.com/thesues/cannyls-go/storage"
)

var (
	path = flag.String("path", "", "path")
)

func snapshotKey() (ret lump.LumpId) {
	//startsWith 0b10XXXXX
	//0x80 + 7 (byte)
	var buf [8]byte
	buf[0] = 0x80
	copy(buf[1:], []byte("snapKey"))
	ret, err := lump.FromBytes(buf[:])
	utils.Check(err)
	return
}

func hardStateKey() (ret lump.LumpId) {
	//startsWith 0b10XXXXX
	//0x80 + 7 (byte)
	var buf [8]byte
	buf[0] = 0x80
	copy(buf[1:], []byte("hardKey"))
	ret, err := lump.FromBytes(buf[:])
	utils.Check(err)
	return
}

var keyMask = (^uint64(0) >> 2) //0x3FFFFFFFFFFFFFFF

func EntryKey(idx uint64) lump.LumpId {
	if idx > keyMask {
		panic("idx is too big")
	}
	idx |= 1 << 63 //first two bit "11"
	idx |= 1 << 62
	return lump.FromU64(0, idx)
}

func ExtKey(idx uint64) lump.LumpId {
	if idx > keyMask {
		panic("idx is too big")
	}
	idx |= 1 << 62 //first two bit "01"
	return lump.FromU64(0, idx)

}

func memberShipKey() (ret lump.LumpId) {
	var buf [8]byte
	buf[0] = 0x80
	copy(buf[1:], []byte("confKey"))
	ret, err := lump.FromBytes(buf[:])
	if err != nil {
		panic("memberShipKey failed")
	}
	return
}

func main() {
	flag.Parse()
	store, err := storage.OpenCannylsStorage(*path)
	utils.Check(err)

	//snapshot key
	fmt.Printf("SNAPSHOT : ")
	data, err := store.Get(snapshotKey())
	if err == nil {
		var snap raftpb.Snapshot
		utils.Check(snap.Unmarshal(data))
		fmt.Printf("%+v\n", snap)
	} else {
		fmt.Printf("\n")
	}
	//

	fmt.Printf("HARDSTAT : ")
	data, err = store.Get(hardStateKey())
	if err == nil {
		var hs raftpb.HardState
		utils.Check(hs.Unmarshal(data))
		fmt.Printf("%+v\n", hs)
	} else {
		fmt.Printf("\n")
	}

	//entries
	wal := raftwal.Init(store)

	fmt.Printf("ENTRIES: \n")
	first, err := wal.FirstIndex()
	fmt.Printf("FirstIndex : %d\n", first)
	last, err := wal.LastIndex()
	fmt.Printf("LastIndex  : %d\n", last)

	es, err := wal.AllEntries(0, raftwal.MaxKey, math.MaxUint64)

	for i := range es {
		switch es[i].Type {
		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(es[i].Data)
			fmt.Printf("index: %d, term: %d , %+v\n", es[i].Index, es[i].Term, cc)
		default:
			fmt.Printf("index: %d, term: %d , %+v\n", es[i].Index, es[i].Term, es[i])
		}

	}

	//applied Data
}
