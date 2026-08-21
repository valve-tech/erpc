package common

import (
	"fmt"
	"os"
	"strings"

	"github.com/grafana/sobek"
)

type Runtime struct {
	vm *sobek.Runtime
}

func NewRuntime() (*Runtime, error) {
	vm := sobek.New()
	vm.SetFieldNameMapper(sobek.TagFieldNameMapper("json", true))

	if err := vm.Set("env", os.Environ()); err != nil {
		return nil, fmt.Errorf("failed to set env vars: %w", err)
	}

	process := map[string]map[string]string{}
	process["env"] = make(map[string]string)

	for _, e := range os.Environ() {
		envKeyValue := strings.SplitN(e, "=", 2)
		process["env"][envKeyValue[0]] = envKeyValue[1]
	}

	global := vm.GlobalObject()

	// Set process.env object
	if err := global.Set("process", vm.ToValue(process)); err != nil {
		return nil, err
	}

	// Set console object
	if err := global.Set("console", console(vm)); err != nil {
		return nil, err
	}

	return &Runtime{vm: vm}, nil
}

func (r *Runtime) VM() *sobek.Runtime {
	return r.vm
}

func (r *Runtime) Evaluate(script string) (sobek.Value, error) {
	return r.vm.RunString(script)
}

// Exports returns the script's `exports` global, or nil when the script
// declares none. A file that forgets `export default` leaves that global
// absent or null, and sobek's ToObject answers a null with a TypeError that
// unwinds into Go as a panic. The caller reports the mistake instead.
func (r *Runtime) Exports() *sobek.Object {
	v := r.vm.GlobalObject().Get("exports")
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return nil
	}
	return v.ToObject(r.vm)
}

func (r *Runtime) ToValue(v interface{}) sobek.Value {
	return r.vm.ToValue(v)
}
