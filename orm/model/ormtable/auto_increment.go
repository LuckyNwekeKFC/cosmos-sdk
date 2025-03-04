package ormtable

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cosmos/cosmos-sdk/orm/encoding/ormkv"
	"github.com/cosmos/cosmos-sdk/orm/types/kv"
	"github.com/cosmos/cosmos-sdk/orm/types/ormerrors"
)

// autoIncrementTable is a Table implementation for tables with an
// auto-incrementing uint64 primary key.
type autoIncrementTable struct {
	*tableImpl
	autoIncField protoreflect.FieldDescriptor
	seqCodec     *ormkv.SeqCodec
}

func (t autoIncrementTable) InsertReturningID(ctx context.Context, message proto.Message) (newId uint64, err error) {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return 0, err
	}

	return t.save(backend, message, saveModeInsert)
}

func (t autoIncrementTable) Save(ctx context.Context, message proto.Message) error {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return err
	}

	_, err = t.save(backend, message, saveModeDefault)
	return err
}

func (t autoIncrementTable) Insert(ctx context.Context, message proto.Message) error {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return err
	}

	_, err = t.save(backend, message, saveModeInsert)
	return err
}

func (t autoIncrementTable) Update(ctx context.Context, message proto.Message) error {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return err
	}

	_, err = t.save(backend, message, saveModeUpdate)
	return err
}

func (t *autoIncrementTable) save(backend Backend, message proto.Message, mode saveMode) (newId uint64, err error) {
	messageRef := message.ProtoReflect()
	val := messageRef.Get(t.autoIncField).Uint()
	writer := newBatchIndexCommitmentWriter(backend)
	defer writer.Close()

	if val == 0 {
		if mode == saveModeUpdate {
			return 0, ormerrors.PrimaryKeyInvalidOnUpdate
		}

		mode = saveModeInsert
		newId, err = t.nextSeqValue(writer.IndexStore())
		if err != nil {
			return 0, err
		}

		messageRef.Set(t.autoIncField, protoreflect.ValueOfUint64(newId))
	} else {
		if mode == saveModeInsert {
			return 0, ormerrors.AutoIncrementKeyAlreadySet
		}

		mode = saveModeUpdate
	}

	return newId, t.tableImpl.doSave(writer, message, mode)
}

func (t *autoIncrementTable) curSeqValue(kv kv.ReadonlyStore) (uint64, error) {
	bz, err := kv.Get(t.seqCodec.Prefix())
	if err != nil {
		return 0, err
	}

	return t.seqCodec.DecodeValue(bz)
}

func (t *autoIncrementTable) nextSeqValue(kv kv.Store) (uint64, error) {
	seq, err := t.curSeqValue(kv)
	if err != nil {
		return 0, err
	}

	seq++
	return seq, t.setSeqValue(kv, seq)
}

func (t *autoIncrementTable) setSeqValue(kv kv.Store, seq uint64) error {
	return kv.Set(t.seqCodec.Prefix(), t.seqCodec.EncodeValue(seq))
}

func (t autoIncrementTable) EncodeEntry(entry ormkv.Entry) (k, v []byte, err error) {
	if _, ok := entry.(*ormkv.SeqEntry); ok {
		return t.seqCodec.EncodeEntry(entry)
	}
	return t.tableImpl.EncodeEntry(entry)
}

func (t autoIncrementTable) ValidateJSON(reader io.Reader) error {
	return t.decodeAutoIncJson(nil, reader, func(message proto.Message, maxID uint64) error {
		messageRef := message.ProtoReflect()
		id := messageRef.Get(t.autoIncField).Uint()
		if id > maxID {
			return fmt.Errorf("invalid ID %d, expected a value <= %d, the highest sequence number", id, maxID)
		}

		if t.customJSONValidator != nil {
			return t.customJSONValidator(message)
		} else {
			return DefaultJSONValidator(message)
		}
	})
}

func (t autoIncrementTable) ImportJSON(ctx context.Context, reader io.Reader) error {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return err
	}

	return t.decodeAutoIncJson(backend, reader, func(message proto.Message, maxID uint64) error {
		messageRef := message.ProtoReflect()
		id := messageRef.Get(t.autoIncField).Uint()
		if id == 0 {
			// we don't have an ID in the JSON, so we call Save to insert and
			// generate one
			_, err = t.save(backend, message, saveModeInsert)
			return err
		} else {
			if id > maxID {
				return fmt.Errorf("invalid ID %d, expected a value <= %d, the highest sequence number", id, maxID)
			}
			// we do have an ID and calling Save will fail because it expects
			// either no ID or SAVE_MODE_UPDATE. So instead we drop one level
			// down and insert using tableImpl which doesn't know about
			// auto-incrementing IDs
			return t.tableImpl.save(backend, message, saveModeInsert)
		}
	})
}

func (t autoIncrementTable) decodeAutoIncJson(backend Backend, reader io.Reader, onMsg func(message proto.Message, maxID uint64) error) error {
	decoder, err := t.startDecodeJson(reader)
	if err != nil {
		return err
	}

	var seq uint64

	return t.doDecodeJson(decoder,
		func(message json.RawMessage) bool {
			err = json.Unmarshal(message, &seq)
			if err == nil {
				// writer is nil during validation
				if backend != nil {
					writer := newBatchIndexCommitmentWriter(backend)
					defer writer.Close()
					err = t.setSeqValue(writer.IndexStore(), seq)
					if err != nil {
						panic(err)
					}
					err = writer.Write()
					if err != nil {
						panic(err)
					}
				}
				return true
			}
			return false
		},
		func(message proto.Message) error {
			return onMsg(message, seq)
		})
}

func (t autoIncrementTable) ExportJSON(ctx context.Context, writer io.Writer) error {
	backend, err := t.getBackend(ctx)
	if err != nil {
		return err
	}

	_, err = writer.Write([]byte("["))
	if err != nil {
		return err
	}

	seq, err := t.curSeqValue(backend.IndexStoreReader())
	if err != nil {
		return err
	}

	if seq != 0 {
		bz, err := json.Marshal(seq)
		if err != nil {
			return err
		}
		_, err = writer.Write(bz)
		if err != nil {
			return err
		}

		_, err = writer.Write([]byte(",\n"))
		if err != nil {
			return err
		}
	}

	return t.doExportJSON(ctx, writer)
}

func (t *autoIncrementTable) GetTable(message proto.Message) Table {
	if message.ProtoReflect().Descriptor().FullName() == t.MessageType().Descriptor().FullName() {
		return t
	}
	return nil
}

var _ AutoIncrementTable = &autoIncrementTable{}
