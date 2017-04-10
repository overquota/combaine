package juggler

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/combaine/combaine/common"
	"github.com/combaine/combaine/common/logger"
	lua "github.com/yuin/gopher-lua"
)

type dumperFunc func(reflect.Value) (lua.LValue, error)

func jPluginConfigToLuaTable(l *lua.LState, in common.PluginConfig) (*lua.LTable, error) {
	table := l.NewTable()
	for name, value := range in {
		val, err := toLuaValue(l, value, dumperToLuaValue)
		if err != nil {
			return nil, err
		}
		table.RawSetString(name, val)
	}
	return table, nil
}

func dataToLuaTable(l *lua.LState, in []common.AggregationResult) (*lua.LTable, error) {
	out := l.NewTable()
	for _, item := range in {
		table := l.NewTable()

		tags := l.NewTable()
		for k, v := range item.Tags {
			tags.RawSetString(k, lua.LString(v))
		}
		table.RawSetString("Tags", tags)

		val, err := toLuaValue(l, item.Result, dumperToLuaNumber)
		if err != nil {
			return nil, err
		}
		table.RawSetString("Result", val)
		out.Append(table)
	}
	return out, nil
}

func toLuaValue(l *lua.LState, v interface{}, dumper dumperFunc) (lua.LValue, error) {
	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if s, ok := v.([]byte); ok {
			return dumper(reflect.ValueOf(fmt.Sprintf("%s", s)))
		}
		inTable := l.NewTable()
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i).Interface()
			v, err := toLuaValue(l, item, dumper)
			if err != nil {
				return nil, err
			}
			inTable.Append(v)
		}
		return inTable, nil
	case reflect.Map:
		inTable := l.NewTable()
		for _, key := range rv.MapKeys() {
			item := rv.MapIndex(key).Interface()
			v, err := toLuaValue(l, item, dumper)
			if err != nil {
				return nil, err
			}
			inTable.RawSetString(fmt.Sprintf("%s", key), v)
		}
		return inTable, nil
	case reflect.Struct:
		inTable := l.NewTable()

		for i := 0; i < rv.NumField(); i++ {
			item := rv.Field(i).Interface()
			v, err := toLuaValue(l, item, dumper)
			if err != nil {
				return nil, err
			}
			inTable.RawSetString(rv.Type().Field(i).Name, v)
		}
		return inTable, nil
	default:
		return dumper(rv)
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
	case reflect.Bool:
		ret = lua.LBool(value.Bool())
	default:
		err = fmt.Errorf("Unexpected value %s: %v", value.Kind(), value)
	}
	return
}

// luaResultToJugglerEvents convert well known type of lua plugin result
// in to go types
func (js *Sender) luaResultToJugglerEvents(result *lua.LTable) ([]jugglerEvent, error) {
	if result == nil {
		return nil, errors.New("lua plugin result is nil")
	}

	errs := make(map[string]string, 0)
	var events []jugglerEvent
	result.ForEach(func(k lua.LValue, v lua.LValue) {
		lt, ok := v.(*lua.LTable)
		if !ok {
			errs[fmt.Sprintf("Failed to convert: result[%s]=%s is not lua table",
				lua.LVAsString(k), lua.LVAsString(v))] = ""
			return
		}
		je := jugglerEvent{}
		tags, ok := lt.RawGetString("tags").(*lua.LTable)
		if !ok {
			errs[fmt.Sprintf("Failed to get tags from lua result[%s]", lua.LVAsString(k))] = ""
			return
		}
		je.Tags = make(map[string]string, 0)
		tags.ForEach(func(tk lua.LValue, tv lua.LValue) {
			je.Tags[lua.LVAsString(tk)] = lua.LVAsString(tv)
		})
		if _, ok := je.Tags["type"]; !ok {
			name := "UnknownEntity"
			if entity, ok := je.Tags["name"]; ok {
				name = entity
			}
			errs[fmt.Sprintf("Missing tag type for %s", name)] = ""
			return
		}
		if je.Description = lua.LVAsString(lt.RawGetString("description")); je.Description == "" {
			je.Description = "no trigger description"
		}
		if je.Service = lua.LVAsString(lt.RawGetString("service")); je.Service == "" {
			je.Service = "UnknownServce"
		}
		if lvl := lt.RawGetString("level"); lvl != lua.LNil {
			je.Level = lua.LVAsString(lvl)
		} else {
			logger.Errf("%s Missing level in %s plugin result, force status to OK", js.id, js.Plugin)
			je.Level = "OK"
			je.Description = fmt.Sprintf("%s (force OK)", je.Description)
		}
		events = append(events, je)
	})
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", errs)
	}
	return events, nil
}

// LoadPlugin cleanup lua state global/local environment
// and load lua plugin by name from juggler config section
func LoadPlugin(id, dir, name string) (*lua.LState, error) {
	file := fmt.Sprintf("%s/%s.lua", dir, name)

	l := lua.NewState()
	if err := PreloadTools(id, l); err != nil {
		return nil, err
	}
	if err := l.DoFile(file); err != nil {
		return nil, err
	}
	// TODO: overwrite/cleanup globals in lua plugin?
	//		 cache lua state in plugin cache?
	return l, nil
}

// preparePluginEnv add data from aggregate task as global variable in lua
// plugin. Also inject juggler conditions from juggler configs and plugin config
func (js *Sender) preparePluginEnv(data []common.AggregationResult) error {
	ltable, err := dataToLuaTable(js.state, data)
	if err != nil {
		return fmt.Errorf("Failed to convert AggregationResult to lua table: %s", err)
	}

	js.state.SetGlobal("payload", ltable)
	js.state.SetGlobal("checkName", lua.LString(js.Config.CheckName))
	js.state.SetGlobal("checkDescription", lua.LString(js.Config.Description))

	// variables
	lvariables := js.state.NewTable()
	for k, v := range js.Config.Variables {
		lvariables.RawSetString(k, lua.LString(v))
	}
	js.state.SetGlobal("variables", lvariables)

	// conditions
	levels := make(map[string][]string)
	if js.OK != nil {
		levels["OK"] = js.OK
	}
	if js.INFO != nil {
		levels["INFO"] = js.INFO
	}
	if js.WARN != nil {
		levels["WARN"] = js.WARN
	}
	if js.CRIT != nil {
		levels["CRIT"] = js.CRIT
	}
	lconditions := js.state.NewTable()
	for name, cond := range levels {
		lcondTable := js.state.NewTable()
		for _, v := range cond {
			lcondTable.Append(lua.LString(v))
		}
		lconditions.RawSetString(name, lcondTable)
	}
	js.state.SetGlobal("conditions", lconditions)

	// config
	lconfig, err := jPluginConfigToLuaTable(js.state, js.JPluginConfig)
	if err != nil {
		return err
	}
	js.state.SetGlobal("config", lconfig)
	return nil
}

// runPlugin run lua plugin with prepared environment
// collect, convert and return plugin result
func (js *Sender) runPlugin() ([]jugglerEvent, error) {
	js.state.Push(js.state.GetGlobal("run"))
	if err := js.state.PCall(0, 1, nil); err != nil {
		return nil, fmt.Errorf("Expected 'run' function inside plugin: %s", err)
	}
	result := js.state.ToTable(1)
	events, err := js.luaResultToJugglerEvents(result)
	if err != nil {
		return nil, err
	}
	return events, nil
}
