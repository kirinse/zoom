package zoom

import (
	"errors"
	"fmt"
	"github.com/stephenalexbrowne/zoom/redis"
	"github.com/stephenalexbrowne/zoom/util"
	"reflect"
)

type transaction struct {
	conn     redis.Conn
	handlers []func(interface{}) error
}

func newTransaction() *transaction {
	t := &transaction{
		conn: GetConn(),
	}
	t.conn.Send("MULTI")
	return t
}

func (t *transaction) command(cmd string, args []interface{}, handler func(interface{}) error) error {
	if err := t.conn.Send(cmd, args...); err != nil {
		t.discard()
		return err
	}
	t.handlers = append(t.handlers, handler)
	return nil
}

func (t *transaction) exec() error {
	defer t.conn.Close()
	replies, err := redis.MultiBulk(t.conn.Do("EXEC"))
	if err != nil {
		t.discard()
		return err
	}
	for i, handler := range t.handlers {
		if handler != nil {
			if err := handler(replies[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *transaction) discard() error {
	defer t.conn.Close()
	_, err := t.conn.Do("DISCARD")
	if err != nil {
		return err
	}
	return nil
}

// useful handlers
func newScanHandler(scannables []interface{}) func(interface{}) error {
	return func(reply interface{}) error {

		// invoke redis driver to scan values into the appropriate scannable
		replies, err := redis.Values(reply, nil)
		if err != nil {
			return err
		}
		if _, err := redis.Scan(replies, scannables...); err != nil {
			return err
		}

		return nil
	}
}

func newScanStructHandler(scannable interface{}) func(interface{}) error {
	return func(reply interface{}) error {

		// invoke redis driver to scan values into the data struct
		bulk, err := redis.MultiBulk(reply, nil)
		if err != nil {
			return err
		}
		if err := redis.ScanStruct(bulk, scannable); err != nil {
			return err
		}

		return nil
	}
}

func newScanSliceHandler(scanVal reflect.Value) func(interface{}) error {
	return func(reply interface{}) error {

		bulk, err := redis.MultiBulk(reply, nil)
		if err != nil {
			return err
		}

		scanType := scanVal.Type()
		scanElem := scanType.Elem()

		for _, el := range bulk {
			srcElem := reflect.ValueOf(el)
			converted := srcElem.Convert(scanElem)
			scanVal.Set(reflect.Append(scanVal, converted))
		}

		return nil
	}
}

// useful operations for transactions
func (t *transaction) saveModel(m Model) error {

	name, err := getRegisteredNameFromInterface(m)
	if err != nil {
		return err
	}

	// set the id if needed
	if m.GetId() == "" {
		m.SetId(generateRandomId())
	}

	// add an operation to write data to database
	key := name + ":" + m.GetId()
	if err := t.saveStruct(key, m); err != nil {
		return err
	}

	// add an operation to add to index for this model
	indexKey := name + ":index"
	if err := t.index(indexKey, m.GetId()); err != nil {
		return err
	}

	// get the modelSpec
	ms, found := modelSpecs[name]
	if !found {
		msg := fmt.Sprintf("zoom: no spec found for model of type %T and registered name %s\n", m, name)
		return errors.New(msg)
	}

	// add operations to save external lists and sets
	if len(ms.lists) != 0 {
		if err := t.saveModelLists(m, name, ms); err != nil {
			return err
		}
	}
	if len(ms.sets) != 0 {
		if err := t.saveModelSets(m, name, ms); err != nil {
			return err
		}
	}

	// add operations to save model relations
	if len(ms.relations) != 0 {
		if err := t.saveModelRelations(m, name, ms); err != nil {
			return err
		}
	}

	return nil
}

func (t *transaction) saveStruct(key string, in interface{}) error {
	args := redis.Args{}.Add(key).AddFlat(in)
	if len(args) > 1 {
		if err := t.command("HMSET", args, nil); err != nil {
			return err
		}
	}

	return nil
}

func (t *transaction) index(key, value string) error {
	args := redis.Args{}.Add(key).Add(value)
	if err := t.command("SADD", args, nil); err != nil {
		return err
	}

	return nil
}

func (t *transaction) saveModelLists(m Model, modelName string, ms *modelSpec) error {
	mVal := reflect.ValueOf(m).Elem()
	for _, list := range ms.lists {
		// use reflection to get the value of field
		field := mVal.FieldByName(list.fieldName)
		if field.IsNil() {
			continue // skip empty lists
		}
		listKey := modelName + ":" + m.GetId() + ":" + list.redisName
		args := redis.Args{}.Add(listKey).AddFlat(field.Interface())
		if err := t.command("RPUSH", args, nil); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) saveModelSets(m Model, modelName string, ms *modelSpec) error {
	mVal := reflect.ValueOf(m).Elem()
	for _, set := range ms.sets {
		// use reflection to get the value of field
		field := mVal.FieldByName(set.fieldName)
		if field.IsNil() {
			continue // skip empty sets
		}
		setKey := modelName + ":" + m.GetId() + ":" + set.redisName
		args := redis.Args{}.Add(setKey).AddFlat(field.Interface())
		if err := t.command("SADD", args, nil); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) saveModelRelations(m Model, modelName string, ms *modelSpec) error {
	for _, relation := range ms.relations {
		if relation.typ == ONE_TO_ONE {
			if err := t.saveModelOneToOneRelation(m, modelName, relation); err != nil {
				return err
			}
		}
	}

	return nil
}

func (t *transaction) saveModelOneToOneRelation(m Model, modelName string, r relation) error {

	// use reflect get the field
	mVal := reflect.ValueOf(m).Elem()
	field := mVal.FieldByName(r.fieldName)

	// make sure its type is registered
	if _, found := typeToName[field.Type()]; !found {
		msg := fmt.Sprintf("zoom: cannot save pointer to a struct of unregistered type %s\n", field.Type().String())
		return errors.New(msg)
	}

	// convert field to a model
	rModel, ok := field.Interface().(Model)
	if !ok {
		msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", field.Type().String())
		return errors.New(msg)
	}

	// add a command to the transaction to set the relation key
	relationKey := modelName + ":" + m.GetId() + ":" + r.redisName
	args := redis.Args{relationKey, rModel.GetId()}
	if err := t.command("SET", args, nil); err != nil {
		return err
	}

	return nil
}

func (t *transaction) findModel(name, id string, scannable Model) error {

	// use HGETALL to get all the fields for the model
	key := name + ":" + id
	if err := t.command("HGETALL", redis.Args{}.Add(key), newScanStructHandler(scannable)); err != nil {
		return err
	}

	// set the model's id
	scannable.SetId(id)

	// get the modelSpec
	ms, found := modelSpecs[name]
	if !found {
		msg := fmt.Sprintf("zoom: no spec found for model of type %T and registered name %s\n", scannable, name)
		return errors.New(msg)
	}

	// find all the external sets and lists for the model
	if len(ms.lists) != 0 {
		if err := t.findModelLists(key, scannable, ms); err != nil {
			return err
		}
	}
	if len(ms.sets) != 0 {
		if err := t.findModelSets(key, scannable, ms); err != nil {
			return err
		}
	}

	// find the relations for the model
	if len(ms.relations) != 0 {
		if err := t.findModelRelations(key, scannable, ms); err != nil {
			return err
		}
	}

	return nil
}

func (t *transaction) findModelWithIncludes(name, id string, scannable Model, includes []string) error {

	// get the appropriate scannable fields
	fields := make([]interface{}, 0)
	modelVal := reflect.ValueOf(scannable).Elem()
	for _, fieldName := range includes {
		fields = append(fields, modelVal.FieldByName(fieldName).Addr().Interface())
	}

	// use HGETALL to get all the fields for the model
	key := name + ":" + id
	args := redis.Args{}.Add(key).AddFlat(includes)
	if err := t.command("HMGET", args, newScanHandler(fields)); err != nil {
		return err
	}

	// set the model's id
	scannable.SetId(id)

	// get the modelSpec
	ms, found := modelSpecs[name]
	if !found {
		msg := fmt.Sprintf("zoom: no spec found for model of type %T and registered name %s\n", scannable, name)
		return errors.New(msg)
	}

	// find all the external sets and lists for the model
	if len(ms.lists) != 0 {
		if err := t.findModelListsWithIncludes(key, scannable, ms, includes); err != nil {
			return err
		}
	}
	if len(ms.sets) != 0 {
		if err := t.findModelSetsWithIncludes(key, scannable, ms, includes); err != nil {
			return err
		}
	}

	return nil
}

func (t *transaction) findModelLists(key string, scannable Model, ms *modelSpec) error {
	for _, list := range ms.lists {
		// use reflection to get a scannable value for the field
		scanVal := reflect.ValueOf(scannable).Elem()
		field := scanVal.FieldByName(list.fieldName)
		// use LRANGE to get all the members of the list
		listKey := key + ":" + list.redisName
		args := redis.Args{listKey, 0, -1}
		if err := t.command("LRANGE", args, newScanSliceHandler(field)); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) findModelSets(key string, scannable Model, ms *modelSpec) error {
	for _, set := range ms.sets {
		// use reflection to get a scannable value for the field
		scanVal := reflect.ValueOf(scannable).Elem()
		field := scanVal.FieldByName(set.fieldName)
		// use SMEMBERS to get all the members of the set
		setKey := key + ":" + set.redisName
		args := redis.Args{setKey}
		if err := t.command("SMEMBERS", args, newScanSliceHandler(field)); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) findModelRelations(key string, scannable Model, ms *modelSpec) error {
	for _, r := range ms.relations {
		if err := t.findModelOneToOneRelation(key, reflect.ValueOf(scannable).Elem(), ms, r); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) findModelOneToOneRelation(key string, modelVal reflect.Value, ms *modelSpec, r relation) error {

	// instantiate the field using reflection
	field := modelVal.FieldByName(r.fieldName)
	field.Set(reflect.New(field.Type().Elem()))

	// get the registered name
	rName, found := typeToName[field.Type()]
	if !found {
		return NewModelTypeNotRegisteredError(field.Type())
	}

	// convert field to a model
	rModel, ok := field.Interface().(Model)
	if !ok {
		msg := fmt.Sprintf("zoom: cannot convert type %s to Model\n", field.Type().String())
		return errors.New(msg)
	}

	// invoke redis driver to get the id
	conn := GetConn()
	defer conn.Close()
	relationKey := key + ":" + r.redisName
	id, err := redis.String(conn.Do("GET", relationKey))
	if err != nil {
		return err
	}

	// add a command to get the model and scan it into the field
	if err := t.findModel(rName, id, rModel); err != nil {
		return err
	}

	return nil
}

func (t *transaction) findModelListsWithIncludes(key string, scannable Model, ms *modelSpec, includes []string) error {
	for _, list := range ms.lists {
		if !util.StringSliceContains(list.fieldName, includes) {
			continue // skip field names that are not in includes
		}
		// use reflection to get a scannable value for the field
		scanVal := reflect.ValueOf(scannable).Elem()
		field := scanVal.FieldByName(list.fieldName)
		// use LRANGE to get all the members of the list
		listKey := key + ":" + list.redisName
		args := redis.Args{listKey, 0, -1}
		if err := t.command("LRANGE", args, newScanSliceHandler(field)); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) findModelSetsWithIncludes(key string, scannable Model, ms *modelSpec, includes []string) error {
	for _, set := range ms.sets {
		if !util.StringSliceContains(set.fieldName, includes) {
			continue // skip field names that are not in includes
		}
		// use reflection to get a scannable value for the field
		scanVal := reflect.ValueOf(scannable).Elem()
		field := scanVal.FieldByName(set.fieldName)
		// use SMEMBERS to get all the members of the set
		setKey := key + ":" + set.redisName
		args := redis.Args{setKey}
		if err := t.command("SMEMBERS", args, newScanSliceHandler(field)); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) deleteModel(modelName, id string) error {

	// add an operation to delete the model itself
	key := modelName + ":" + id
	if err := t.delete(key); err != nil {
		return err
	}

	// add an operation to remove the model id from the index
	indexKey := modelName + ":index"
	if err := t.unindex(indexKey, id); err != nil {
		return err
	}

	return nil
}

func (t *transaction) delete(key string) error {
	if err := t.command("DEL", redis.Args{}.Add(key), nil); err != nil {
		return err
	}

	return nil
}

func (t *transaction) unindex(key, value string) error {
	args := redis.Args{}.Add(key).Add(value)
	if err := t.command("SREM", args, nil); err != nil {
		return err
	}

	return nil
}
