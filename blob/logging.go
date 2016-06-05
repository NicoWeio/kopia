package blob

import "log"

type loggingStorage struct {
	Storage
}

func (s *loggingStorage) BlockExists(id string) (bool, error) {
	result, err := s.Storage.BlockExists(id)
	log.Printf("BlockExists(%#v)=%#v,%#v", id, result, err)
	return result, err
}

func (s *loggingStorage) GetBlock(id string) ([]byte, error) {
	result, err := s.Storage.GetBlock(id)
	if len(result) < 20 {
		log.Printf("GetBlock(%#v)=(%#v, %#v)", id, result, err)
	} else {
		log.Printf("GetBlock(%#v)=({%#v bytes}, %#v)", id, len(result), err)
	}
	return result, err
}

func (s *loggingStorage) PutBlock(id string, data BlockReader, options PutOptions) error {
	err := s.Storage.PutBlock(id, data, options)
	log.Printf("PutBlock(%#v, %#v, %#v bytes)=%#v", id, options, data.Len(), err)
	return err
}

func (s *loggingStorage) DeleteBlock(id string) error {
	err := s.Storage.DeleteBlock(id)
	log.Printf("DeleteBlock(%#v)=%#v", id, err)
	return err
}

func (s *loggingStorage) ListBlocks(prefix string) chan (BlockMetadata) {
	log.Printf("ListBlocks(%#v)", prefix)
	return s.Storage.ListBlocks(prefix)
}

func (s *loggingStorage) Flush() error {
	log.Printf("Flush()")
	return s.Storage.Flush()
}

// NewLoggingWrapper returns a Storage wrapper that logs all storage commands.
func NewLoggingWrapper(wrapped Storage) Storage {
	return &loggingStorage{wrapped}
}
