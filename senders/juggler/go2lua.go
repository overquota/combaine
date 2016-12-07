package juggler

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/combaine/combaine/common/configs"
	"github.com/combaine/combaine/common/tasks"
	lua "github.com/yuin/gopher-lua"
)

type DumperFunc func(reflect.Value) (lua.LValue, error)

type JugglerLevels map[string]int

var jLevels = JugglerLevels{
	"OK":   0,
	"WARN": 1,
	"CRIT": 2,
	"INFO": 3,
}

func jPluginConfigToLuaTable(l *lua.LState, in configs.PluginConfig) (*lua.LTable, error) {
	table := l.NewTable()
	for name, value := range in {
		if val, err := ToLuaValue(l, value, dumperToLuaValue); err == nil {
			table.RawSetString(name, val)
		} else {
			return nil, err
		}
	}
	return table, nil
}

func dataToLuaTable(l *lua.LState, in tasks.DataType) (*lua.LTable, error) {
	out := l.NewTable()
	for host, item := range in {
		table := l.NewTable()
		out.RawSetString(host, table)
		for metric, value := range item {
			if val, err := ToLuaValue(l, value, dumperToLuaNumber); err == nil {
				table.RawSetString(metric, val)
			} else {
				return nil, err
			}
		}
	}
	return out, nil
}

func ToLuaValue(l *lua.LState, v interface{}, dumper DumperFunc) (lua.LValue, error) {
	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		inTable := l.NewTable()
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i).Interface()
			if v, err := ToLuaValue(l, item, dumper); err == nil {
				inTable.Append(v)
			} else {
				return nil, err
			}
		}
		return inTable, nil
	case reflect.Map:
		inTable := l.NewTable()
		for _, key := range rv.MapKeys() {
			item := rv.MapIndex(key).Interface()
			if v, err := ToLuaValue(l, item, dumper); err == nil {
				inTable.RawSetString(key.String(), v)
			} else {
				return nil, err
			}
		}
		return inTable, nil
	case reflect.Struct:
		inTable := l.NewTable()

		for i := 0; i < rv.NumField(); i++ {
			item := rv.Field(i).Interface()
			if v, err := ToLuaValue(l, item, dumper); err == nil {
				inTable.RawSetString(rv.Type().Field(i).Name, v)
			} else {
				return nil, err
			}
		}
		return inTable, nil
	default:
		if v, err := dumper(rv); err == nil {
			return v, nil
		} else {
			return nil, err
		}
	}
}

func dumperToLuaNumber(value reflect.Value) (ret lua.LValue, err error) {
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		ret = lua.LNumber(value.Float())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ret = lua.LNumber(float64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		ret = lua.LNumber(float64(value.Uint()))
	case reflect.String:
		var num float64
		num, err = strconv.ParseFloat(value.String(), 64)
		if err == nil {
			ret = lua.LNumber(num)
		}
	default:
		err = fmt.Errorf("value %s is Not a Number: %v", value.Kind(), value)
	}
	return
}

func dumperToLuaValue(value reflect.Value) (ret lua.LValue, err error) {
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		ret = lua.LNumber(value.Float())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ret = lua.LNumber(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		ret = lua.LNumber(value.Uint())
	case reflect.String:
		ret = lua.LString(value.String())
	default:
		err = fmt.Errorf("value %s is Not a Number or String: %v", value.Kind(), value)
	}
	return
}

// luaResultToJugglerEvents convert well known type of lua plugin result
// in to go types
func luaResultToJugglerEvents(defaultLevel string, result *lua.LTable) ([]jugglerEvent, error) {
	if defaultLevel == "" {
		defaultLevel = DEFAULT_CHECK_LEVEL
	}
	if result == nil {
		return nil, errors.New("lua plugin result is nil")
	}

	var errs []error
	var events []jugglerEvent
	result.ForEach(func(k lua.LValue, v lua.LValue) {
		lt, ok := v.(*lua.LTable)
		if !ok {
			errs = append(errs, fmt.Errorf("Failed to convert: result[%s]=%s is not lua table",
				lua.LVAsString(k), lua.LVAsString(v)))
		}
		je := jugglerEvent{}
		if je.Host = lua.LVAsString(lt.RawGetString("host")); je.Host == "" {
			errs = append(errs, errors.New("UnknownHost in event"))
			return
		}
		if je.Description = lua.LVAsString(lt.RawGetString("description")); je.Description == "" {
			je.Description = "no trigger description"
		}
		if je.Service = lua.LVAsString(lt.RawGetString("service")); je.Service == "" {
			je.Service = "UnknownServce"
		}
		level := lua.LVAsString(lt.RawGetString("level"))
		if l, ok := jLevels[level]; ok {
			je.Level = l
		} else {
			je.Level = jLevels[defaultLevel]
			je.Description = fmt.Sprintf("%s (Forse status %s)", je.Description, defaultLevel)
		}

		events = append(events, je)
	})
	if errs != nil {
		return nil, fmt.Errorf("%s", errs)
	}
	return events, nil
}
