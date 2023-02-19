package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Inphi/eip4844-interop/shared"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
	ssz "github.com/prysmaticlabs/fastssz"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/p2p/encoder"
	p2ptypes "github.com/prysmaticlabs/prysm/v3/beacon-chain/p2p/types"
	"github.com/prysmaticlabs/prysm/v3/beacon-chain/sync"
	consensustypes "github.com/prysmaticlabs/prysm/v3/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v3/consensus-types/wrapper"
	ethpb "github.com/prysmaticlabs/prysm/v3/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v3/proto/prysm/v1alpha1/metadata"
)

var responseCodeSuccess = byte(0x00)

func SendBlobsSidecarsByRangeRequest(ctx context.Context, h host.Host, encoding encoder.NetworkEncoding, pid peer.ID, req *ethpb.BlobsSidecarsByRangeRequest) ([]*ethpb.BlobsSidecar, error) {
	topic := fmt.Sprintf("%s%s", p2p.RPCBlobsSidecarsByRangeTopicV1, encoding.ProtocolSuffix())

	stream, err := h.NewStream(ctx, pid, protocol.ID(topic))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = stream.Close()
	}()

	if _, err := encoding.EncodeWithMaxLength(stream, req); err != nil {
		_ = stream.Reset()
		return nil, err
	}

	if err := stream.CloseWrite(); err != nil {
		_ = stream.Reset()
		return nil, err
	}

	var blobsSidecars []*ethpb.BlobsSidecar
	for {
		isFirstChunk := len(blobsSidecars) == 0
		blobs, err := readChunkedBlobsSidecar(stream, encoding, isFirstChunk)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		blobsSidecars = append(blobsSidecars, blobs)
	}
	return blobsSidecars, nil
}

func readChunkedBlobsSidecar(stream network.Stream, encoding encoder.NetworkEncoding, isFirstChunk bool) (*ethpb.BlobsSidecar, error) {
	var (
		code   uint8
		errMsg string
		err    error
	)
	if isFirstChunk {
		code, errMsg, err = sync.ReadStatusCode(stream, encoding)
	} else {
		sync.SetStreamReadDeadline(stream, time.Second*10)
		code, errMsg, err = readStatusCodeNoDeadline(stream, encoding)
	}
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New(errMsg)
	}
	// ignored: we assume we got the correct context
	b := make([]byte, 4)
	if _, err := stream.Read(b); err != nil {
		return nil, err
	}
	sidecar := new(ethpb.BlobsSidecar)
	err = encoding.DecodeWithMaxLength(stream, sidecar)
	return sidecar, err
}

func readStatusCodeNoDeadline(stream network.Stream, encoding encoder.NetworkEncoding) (uint8, string, error) {
	b := make([]byte, 1)
	_, err := stream.Read(b)
	if err != nil {
		return 0, "", err
	}
	if b[0] == responseCodeSuccess {
		return 0, "", nil
	}
	msg := &p2ptypes.ErrorMessage{}
	if err := encoding.DecodeWithMaxLength(stream, msg); err != nil {
		return 0, "", err
	}
	return b[0], string(*msg), nil
}

// Using p2p RPC
func DownloadBlobs(ctx context.Context, startSlot consensustypes.Slot, count uint64, beaconMA string) []byte {
	log.Print("downloading blobs...")

	req := &ethpb.BlobsSidecarsByRangeRequest{
		StartSlot: startSlot,
		Count:     count,
	}

	h, err := libp2p.New(libp2p.Transport(tcp.NewTCPTransport))
	if err != nil {
		log.Fatalf("failed to create libp2p context: %v", err)
	}
	defer func() {
		_ = h.Close()
	}()
	// h.RemoveStreamHandler(identify.IDDelta)
	// setup enough handlers so lighthouse thinks it's dealing with a beacon peer
	setHandler(h, p2p.RPCPingTopicV1, pingHandler)
	setHandler(h, p2p.RPCGoodByeTopicV1, pingHandler)
	setHandler(h, p2p.RPCMetaDataTopicV1, pingHandler)
	setHandler(h, p2p.RPCMetaDataTopicV2, pingHandler)

	nilHandler := func(ctx context.Context, i interface{}, stream network.Stream) error {
		log.Printf("received request for %s", stream.Protocol())
		return nil
	}
	setHandler(h, p2p.RPCBlocksByRangeTopicV1, nilHandler)
	setHandler(h, p2p.RPCBlocksByRangeTopicV2, nilHandler)
	setHandler(h, p2p.RPCBlobsSidecarsByRangeTopicV1, nilHandler)

	maddr, err := ma.NewMultiaddr(beaconMA)
	if err != nil {
		log.Fatalf("failed to get multiaddr: %v", err)
	}
	addrInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		log.Fatalf("failed to get addr info: %v", err)
	}

	err = h.Connect(ctx, *addrInfo)
	if err != nil {
		log.Fatalf("libp2p host connect: %v", err)
	}

	sidecars, err := SendBlobsSidecarsByRangeRequest(ctx, h, encoder.SszNetworkEncoder{}, addrInfo.ID, req)
	if err != nil {
		log.Fatalf("failed to send blobs p2p request: %v", err)
	}

	anyBlobs := false
	blobsBuffer := new(bytes.Buffer)
	for _, sidecar := range sidecars {
		if sidecar.Blobs == nil || len(sidecar.Blobs) == 0 {
			continue
		}
		anyBlobs = true
		for _, blob := range sidecar.Blobs {
			data := shared.DecodeFlatBlob(blob.Data)
			_, _ = blobsBuffer.Write(data)
		}

		// stop after the first sidecar with blobs:
		break
	}
	if !anyBlobs {
		log.Fatalf("No blobs found in requested slots, sidecar count: %d", len(sidecars))
	}

	return blobsBuffer.Bytes()
}

func getMultiaddr(ctx context.Context, h host.Host, addr string) (ma.Multiaddr, error) {
	multiaddr, err := ma.NewMultiaddr(addr)
	if err != nil {
		return nil, err
	}
	_, id := peer.SplitAddr(multiaddr)
	if id != "" {
		return multiaddr, nil
	}
	// peer ID wasn't provided, look it up
	id, err = retrievePeerID(ctx, h, addr)
	if err != nil {
		return nil, err
	}
	return ma.NewMultiaddr(fmt.Sprintf("%s/p2p/%s", addr, string(id)))
}

// Helper for retrieving the peer ID from a security error... obviously don't use this in production!
// See https://github.com/libp2p/go-libp2p-noise/blob/v0.3.0/handshake.go#L250
func retrievePeerID(ctx context.Context, h host.Host, addr string) (peer.ID, error) {
	incorrectPeerID := "16Uiu2HAmSifdT5QutTsaET8xqjWAMPp4obrQv7LN79f2RMmBe3nY"
	addrInfo, err := peer.AddrInfoFromString(fmt.Sprintf("%s/p2p/%s", addr, incorrectPeerID))
	if err != nil {
		return "", err
	}
	err = h.Connect(ctx, *addrInfo)
	if err == nil {
		return "", errors.New("unexpected successful connection")
	}
	if strings.Contains(err.Error(), "but remote key matches") {
		split := strings.Split(err.Error(), " ")
		return peer.ID(split[len(split)-1]), nil
	}
	return "", err
}

type rpcHandler func(context.Context, interface{}, network.Stream) error

// adapted from prysm's handler router
func setHandler(h host.Host, baseTopic string, handler rpcHandler) {
	encoding := &encoder.SszNetworkEncoder{}
	topic := baseTopic + encoding.ProtocolSuffix()
	h.SetStreamHandler(protocol.ID(topic), func(stream network.Stream) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Panic occurred: %v", r)
				log.Printf("%s", debug.Stack())
			}
		}()

		// Resetting after closing is a no-op so defer a reset in case something goes wrong.
		// It's up to the handler to Close the stream (send an EOF) if
		// it successfully writes a response. We don't blindly call
		// Close here because we may have only written a partial
		// response.
		defer func() {
			_err := stream.Reset()
			_ = _err
		}()

		base, ok := p2p.RPCTopicMappings[baseTopic]
		if !ok {
			log.Printf("ERROR: Could not retrieve base message for topic %s", baseTopic)
			return
		}
		bb := base
		t := reflect.TypeOf(base)
		// Copy Base
		base = reflect.New(t)

		if baseTopic == p2p.RPCMetaDataTopicV1 || baseTopic == p2p.RPCMetaDataTopicV2 {
			if err := metadataHandler(context.Background(), base, stream); err != nil {
				if err != p2ptypes.ErrWrongForkDigestVersion {
					log.Printf("ERROR: Could not handle p2p RPC: %v", err)
				}
			}
			return
		}

		// Given we have an input argument that can be pointer or the actual object, this gives us
		// a way to check for its reflect.Kind and based on the result, we can decode
		// accordingly.
		if t.Kind() == reflect.Ptr {
			msg, ok := reflect.New(t.Elem()).Interface().(ssz.Unmarshaler)
			if !ok {
				log.Printf("ERROR: message of %T ptr does not support marshaller interface. topic=%s", bb, baseTopic)
				return
			}
			if err := encoding.DecodeWithMaxLength(stream, msg); err != nil {
				log.Printf("ERROR: could not decode stream message: %v", err)
				return
			}
			if err := handler(context.Background(), msg, stream); err != nil {
				if err != p2ptypes.ErrWrongForkDigestVersion {
					log.Printf("ERROR: Could not handle p2p RPC: %v", err)
				}
			}
		} else {
			nTyp := reflect.New(t)
			msg, ok := nTyp.Interface().(ssz.Unmarshaler)
			if !ok {
				log.Printf("ERROR: message of %T does not support marshaller interface", msg)
				return
			}
			if err := handler(context.Background(), msg, stream); err != nil {
				if err != p2ptypes.ErrWrongForkDigestVersion {
					log.Printf("ERROR: Could not handle p2p RPC: %v", err)
				}
			}
		}
	})
}

func dummyMetadata() metadata.Metadata {
	metaData := &ethpb.MetaDataV1{
		SeqNumber: 0,
		Attnets:   bitfield.NewBitvector64(),
		Syncnets:  bitfield.Bitvector4{byte(0x00)},
	}
	return wrapper.WrappedMetadataV1(metaData)
}

// pingHandler reads the incoming ping rpc message from the peer.
func pingHandler(_ context.Context, _ interface{}, stream network.Stream) error {
	encoding := &encoder.SszNetworkEncoder{}
	defer closeStream(stream)
	if _, err := stream.Write([]byte{responseCodeSuccess}); err != nil {
		return err
	}
	m := dummyMetadata()
	sq := consensustypes.SSZUint64(m.SequenceNumber())
	if _, err := encoding.EncodeWithMaxLength(stream, &sq); err != nil {
		return fmt.Errorf("%w: pingHandler stream write", err)
	}
	return nil
}

// metadataHandler spoofs a valid looking metadata message
func metadataHandler(_ context.Context, _ interface{}, stream network.Stream) error {
	encoding := &encoder.SszNetworkEncoder{}
	defer closeStream(stream)
	if _, err := stream.Write([]byte{responseCodeSuccess}); err != nil {
		return err
	}

	// write a dummy metadata message to satify the client handshake
	m := dummyMetadata()
	if _, err := encoding.EncodeWithMaxLength(stream, m); err != nil {
		return fmt.Errorf("%w: metadata stream write", err)
	}
	return nil
}

func closeStream(stream network.Stream) {
	if err := stream.Close(); err != nil {
		log.Println(err)
	}
}
