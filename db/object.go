package db

import (
	"context"
	api "github.com/autom8ter/geodb/gen/go/geodb"
	"github.com/autom8ter/geodb/helpers"
	"github.com/autom8ter/geodb/maps"
	"github.com/autom8ter/geodb/stream"
	"github.com/dgraph-io/badger/v2"
	"github.com/gogo/protobuf/proto"
	geo "github.com/paulmach/go.geo"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"regexp"
	"time"
)

func Set(db *badger.DB, maps *maps.Client, hub *stream.Hub, objs map[string]*api.Object) map[string]*api.ObjectDetail {
	var objects = map[string]*api.ObjectDetail{}
	for key, val := range objs {
		txn := db.NewTransaction(true)
		defer txn.Discard()
		val.Key = key
		if val.UpdatedUnix == 0 {
			val.UpdatedUnix = time.Now().Unix()
		}
		point1 := geo.NewPointFromLatLng(val.Point.Lat, val.Point.Lon)
		var events = map[string]*api.Tracker{}
		if len(val.Trackers) > 0 {
			for _, tracker := range val.Trackers {
				item, err := txn.Get([]byte(tracker))
				if err != nil {
					log.Error(err.Error())
					continue
				}
				res, err := item.ValueCopy(nil)
				if err != nil {
					log.Error(err.Error())
					continue
				}
				var obj = &api.Object{}
				if err := proto.Unmarshal(res, obj); err != nil {
					log.Error(err.Error())
					continue
				}
				if obj.Point == nil {
					continue
				}
				point2 := geo.NewPointFromLatLng(obj.Point.Lat, obj.Point.Lon)
				dist := point1.GeoDistanceFrom(point2, true)
				event := &api.Tracker{
					Object:        obj,
					Distance:      dist,
					Inside:        dist <= float64(val.Radius+obj.Radius),
					TimestampUnix: val.UpdatedUnix,
				}
				if maps != nil {
					directions, eta, dist, err := maps.TravelDetail(context.Background(), val.Point, obj.Point, helpers.ToTravelMode(val.TravelMode))
					if err != nil {
						log.Error(err.Error())
					} else {
						event.Direction = &api.Directions{
							HtmlDirections: directions,
							Eta:            int64(eta),
							TravelDist:     int64(dist),
						}
					}
				}
				events[obj.Key] = event
			}
		}
		detail := &api.ObjectDetail{
			Object: val,
		}
		if maps != nil {
			addr, err := maps.GetAddress(val.Point)
			if err != nil {
				log.Error(err.Error())
			}
			detail.Address = addr
		}
		if len(events) > 0 {
			for _, event := range events {
				detail.Trackers = append(detail.Trackers, event)
			}
		}

		bits, _ := proto.Marshal(detail)
		if err := txn.SetEntry(&badger.Entry{
			Key:       []byte(key),
			Value:     bits,
			UserMeta:  1,
			ExpiresAt: uint64(val.ExpiresUnix),
		}); err != nil {
			log.Error(err.Error())
		}
		if err := txn.Commit(); err != nil {
			log.Error(err.Error())
			continue
		}
		hub.PublishObject(detail)
		objects[detail.Object.Key] = detail
	}
	return objects
}

func Get(db *badger.DB, keys []string) (map[string]*api.ObjectDetail, error) {
	txn := db.NewTransaction(false)
	defer txn.Discard()
	objects := map[string]*api.ObjectDetail{}
	if len(keys) == 0 {
		iter := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iter.Close()
		for iter.Rewind(); iter.Valid(); iter.Next() {
			item := iter.Item()
			res, err := item.ValueCopy(nil)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to copy data: %s", err.Error())
			}
			if len(res) > 0 {
				var obj = &api.ObjectDetail{}
				if err := proto.Unmarshal(res, obj); err != nil {
					return nil, status.Errorf(codes.Internal, "failed to unmarshal protobuf: %s", err.Error())
				}
				objects[string(item.Key())] = obj
			}
		}
	} else {
		for _, key := range keys {
			i, err := txn.Get([]byte(key))
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "failed to get key: %s", err.Error())
			}
			if i.UserMeta() != 1 {
				continue
			}
			res, err := i.ValueCopy(nil)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to copy data: %s", err.Error())
			}
			var obj = &api.ObjectDetail{}
			if err := proto.Unmarshal(res, obj); err != nil {
				return nil, status.Errorf(codes.Internal, "failed to unmarshal protobuf: %s", err.Error())
			}
			objects[key] = obj
		}
	}
	return objects, nil
}

func GetRegex(db *badger.DB, regex string) (map[string]*api.ObjectDetail, error) {
	txn := db.NewTransaction(false)
	defer txn.Discard()
	objects := map[string]*api.ObjectDetail{}
	iter := txn.NewIterator(badger.DefaultIteratorOptions)
	defer iter.Close()
	for iter.Rewind(); iter.Valid(); iter.Next() {
		item := iter.Item()
		if item.UserMeta() != 1 {
			continue
		}
		match, err := regexp.MatchString(regex, string(item.Key()))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to match regex: %s", err.Error())
		}
		if match {
			res, err := item.ValueCopy(nil)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to copy data: %s", err.Error())
			}
			var obj = &api.ObjectDetail{}
			if err := proto.Unmarshal(res, obj); err != nil {
				return nil, status.Errorf(codes.Internal, "failed to unmarshal protobuf: %s", err.Error())
			}
			objects[string(item.Key())] = obj
		}

	}
	return objects, nil
}

func GetPrefix(db *badger.DB, prefix string) (map[string]*api.ObjectDetail, error) {
	txn := db.NewTransaction(false)
	defer txn.Discard()
	objects := map[string]*api.ObjectDetail{}
	iter := txn.NewIterator(badger.DefaultIteratorOptions)
	defer iter.Close()
	for iter.Seek([]byte(prefix)); iter.ValidForPrefix([]byte(prefix)); iter.Next() {
		item := iter.Item()
		if item.UserMeta() != 1 {
			continue
		}
		res, err := item.ValueCopy(nil)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to copy data: %s", err.Error())
		}
		var obj = &api.ObjectDetail{}
		if err := proto.Unmarshal(res, obj); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to unmarshal protobuf: %s", err.Error())
		}
		objects[string(item.Key())] = obj
	}
	return objects, nil
}

func Delete(db *badger.DB, keys []string) error {
	txn := db.NewTransaction(true)
	defer txn.Discard()
	if len(keys) > 0 && keys[0] == "*" {
		iter := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iter.Close()
		for iter.Rewind(); iter.Valid(); iter.Next() {
			item := iter.Item()
			if err := txn.Delete(item.Key()); err != nil {
				return status.Errorf(codes.Internal, "failed to delete key: %s %s", string(item.Key()), err.Error())
			}
		}
	} else {
		for _, key := range keys {
			if err := txn.Delete([]byte(key)); err != nil {
				return status.Errorf(codes.Internal, "failed to delete key: %s %s", key, err.Error())
			}
		}
	}
	if err := txn.Commit(); err != nil {
		return status.Errorf(codes.Internal, "failed to delete keys %s", err.Error())
	}
	return nil
}
