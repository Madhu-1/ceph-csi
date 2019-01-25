package main

import (
	"fmt"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
)

func timestampToUnixTime(t *timestamp.Timestamp) (int64, error) {
	time, err := ptypes.Timestamp(t)
	if err != nil {
		return -1, err
	}
	// TODO: clean this up, we probably don't need this translation layer
	// and can just use time.Time
	return time.UnixNano(), nil
}

func main() {
	t := &timestamp.Timestamp{
		Seconds: ptypes.TimestampNow().GetSeconds(),
	}
	a, e := timestampToUnixTime(t)
fmt.Println("error in conversion",e)
fmt.Println(a)
}
