// Copyright (C) 2019 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package protocol

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"time"

	"github.com/gogo/protobuf/proto"
	"golang.org/x/crypto/nacl/secretbox"
)

const blockOverhead = secretbox.Overhead + 24 // Nonce is [24]byte and prepended to each block

// The encryptedModel sits between the encrypted device and the model. It
// receives encrypted metadata and requests, so it must decrypt those and answer
// requests by encrypting the data.
type encryptedModel struct {
	Model
	key *[32]byte
}

func (e encryptedModel) Index(deviceID DeviceID, folder string, files []FileInfo) error {
	if err := decryptFileInfos(files, e.key); err != nil {
		return err
	}
	return e.Model.Index(deviceID, folder, files)
}

func (e encryptedModel) IndexUpdate(deviceID DeviceID, folder string, files []FileInfo) error {
	if err := decryptFileInfos(files, e.key); err != nil {
		return err
	}
	return e.Model.IndexUpdate(deviceID, folder, files)
}

func (e encryptedModel) Request(deviceID DeviceID, folder, name string, size int32, offset int64, hash []byte, weakHash uint32, fromTemporary bool) (RequestResponse, error) {
	// Dededuct the real file, offset and size from the encrypted values.

	realName, err := decryptName(name, e.key)
	if err != nil {
		return nil, err
	}
	realSize := size - blockOverhead
	realOffset := offset - int64(weakHash*blockOverhead)

	// Perform that request and grab the data.

	resp, err := e.Model.Request(deviceID, folder, realName, realSize, realOffset, nil, 0, false)
	if err != nil {
		if resp != nil {
			resp.Close()
		}
		return nil, err
	}
	data := resp.Data()
	enc := encryptResponse(data, e.key)
	resp.Close()
	return rawResponse{enc}, nil
}

func (e encryptedModel) DownloadProgress(deviceID DeviceID, folder string, updates []FileDownloadProgressUpdate) error {
	// The updates contain nonsense names and sizes, so we ignore them.
	return nil
}

// The encryptedConnection sits between the model and the encrypted device. It
// encrypts outgoing metadata and decrypts incoming responses.
type encryptedConnection struct {
	Connection
	key *[32]byte
}

func (e encryptedConnection) Index(ctx context.Context, folder string, files []FileInfo) error {
	encryptFileInfos(files, e.key)
	return e.Connection.Index(ctx, folder, files)
}

func (e encryptedConnection) IndexUpdate(ctx context.Context, folder string, files []FileInfo) error {
	encryptFileInfos(files, e.key)
	return e.Connection.IndexUpdate(ctx, folder, files)
}

func (e encryptedConnection) Request(ctx context.Context, folder string, name string, blockNo int, offset int64, size int, hash []byte, weakHash uint32, fromTemporary bool) ([]byte, error) {
	name = encryptName(name, e.key)
	realOffset := offset
	offset += int64(blockNo * blockOverhead)
	realSize := size
	size += blockOverhead

	bs, err := e.Connection.Request(ctx, folder, name, blockNo, offset, size, nil, uint32(blockNo), false)
	if err != nil {
		return nil, err
	}

	return decryptResponse(bs, e.key)
}

func (e encryptedConnection) DownloadProgress(ctx context.Context, folder string, updates []FileDownloadProgressUpdate) {
	return
}

func encryptFileInfos(files []FileInfo, key *[32]byte) error {
	for i, fi := range files {
		files[i] = encryptFileInfo(fi, key)
	}
	return nil
}

func encryptFileInfo(fi FileInfo, key *[32]byte) FileInfo {
	// The entire FileInfo is encrypted with a random nonce, and concatenated
	// with that nonce.

	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic("catastrophic randomness failure: " + err.Error())
	}
	bs, err := proto.Marshal(&fi)
	if err != nil {
		panic("impossible serialization mishap: " + err.Error())
	}
	encryptedFI := secretbox.Seal(nonce[:], bs, &nonce, key)

	// The vector is set to something that is higher than any other version sent
	// previously, assuming people's clocks are correct. We do this because
	// there is no way for the insecure device on the other end to do proper
	// conflict resolution, so they will simply accept and keep whatever is the
	// latest version they see. The secure devices will decrypt the real
	// FileInfo, see the real Version, and act appropriately regardless of what
	// this fake version happens to be.

	version := Vector{
		Counters: []Counter{
			{
				ID:    1,
				Value: uint64(time.Now().UnixNano()),
			},
		},
	}

	// Construct the fake block list. Each block will be blockOverhead
	// bytes larger than the corresponding real one, have a nil hash and the
	// block number in the weak hash. Stuffing the block number in the weak hash
	// is an ugly hack that avoids a couple of other protocol changes, as it is
	// a value that is propagated through to the block request. It helps the
	// other end figure out the actual block offset to look at, given that the
	// offset we get from the encrypted side is tainted by an unknown number of
	// blockOverheads.

	var offset int64
	var blocks []BlockInfo
	for i, b := range fi.Blocks {
		size := b.Size + blockOverhead
		blocks = append(blocks, BlockInfo{
			Offset:   offset,
			Size:     size,
			WeakHash: uint32(i),
		})
		offset += int64(size)
	}

	// Construct the fake FileInfo. This is mostly just a wrapper around the
	// encrypted FileInfo and fake block list.

	typ := FileInfoTypeFile
	if fi.Type != FileInfoTypeFile {
		typ = FileInfoTypeDirectory
	}
	enc := FileInfo{
		Name:         encryptName(fi.Name, key),
		Type:         typ,
		Size:         offset,
		Permissions:  0644,
		ModifiedS:    1234567890, // Sat Feb 14 00:31:30 CET 2009
		Deleted:      fi.Deleted,
		Version:      version,
		Sequence:     fi.Sequence,
		RawBlockSize: fi.RawBlockSize + blockOverhead,
		Blocks:       blocks,
		Encrypted:    encryptedFI,
	}

	return enc
}

func decryptFileInfos(files []FileInfo, key *[32]byte) error {
	for i, fi := range files {
		decFI, err := decryptFileInfo(fi, key)
		if err != nil {
			return err
		}
		files[i] = decFI
	}
	return nil
}

func decryptFileInfo(fi FileInfo, key *[32]byte) (FileInfo, error) {
	if len(fi.Encrypted) < 24 {
		return FileInfo{}, errors.New("FileInfo too short")
	}

	var nonce [24]byte
	copy(nonce[:], fi.Encrypted)
	dec, ok := secretbox.Open(nil, fi.Encrypted[24:], &nonce, key)
	if !ok {
		return FileInfo{}, errors.New("FileInfo decryption failed")
	}

	var decFI FileInfo
	if err := proto.Unmarshal(dec, &decFI); err != nil {
		return FileInfo{}, err
	}
	return decFI, nil
}

func encryptName(name string, key *[32]byte) string {
	h := sha256.Sum256(append([]byte(name), (*key)[:]...))
	var nonce [24]byte
	copy(nonce[:], h[:])
	enc := secretbox.Seal(nonce[:], []byte(name), &nonce, key)
	return base32.HexEncoding.EncodeToString(enc)
}

func decryptName(name string, key *[32]byte) (string, error) {
	bs, err := base32.HexEncoding.DecodeString(name)
	if err != nil {
		return "", err
	}
	if len(bs) < 24 {
		return "", errors.New("name too short")
	}

	var nonce [24]byte
	copy(nonce[:], bs)
	dec, ok := secretbox.Open(nil, bs[24:], &nonce, key)
	if !ok {
		return "", errors.New("name decryption failed")
	}

	return string(dec), nil
}

func encryptResponse(data []byte, key *[32]byte) []byte {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic("catastrophic randomness failure: " + err.Error())
	}

	return secretbox.Seal(nonce[:], data, &nonce, key)
}

func decryptResponse(data []byte, key *[32]byte) ([]byte, error) {
	if len(data) < 24 {
		return nil, errors.New("response too short")
	}

	var nonce [24]byte
	copy(nonce[:], data)
	dec, ok := secretbox.Open(nil, data[24:], &nonce, key)
	if !ok {
		return nil, errors.New("response decryption failed")
	}

	return dec, nil
}

type rawResponse struct {
	data []byte
}

func (r rawResponse) Data() []byte {
	return r.data
}

func (r rawResponse) Close() {}
func (r rawResponse) Wait()  {}