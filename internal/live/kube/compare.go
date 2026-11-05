package kube

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/api/resource"
)

// inSync reports whether the live object has everything the desired one
// says: every field desired sets, live has with the same value. Fields the
// API server fills in (defaults, status, uid, managed fields) are not
// desired, so they never count as drift. drift names the first difference.
func inSync(desired, live map[string]any) (ok bool, drift string) {
	for k, dv := range desired {
		switch k {
		case "status":
			continue
		case "metadata":
			dm, _ := dv.(map[string]any)
			lm, _ := live["metadata"].(map[string]any)
			for _, f := range []string{"name", "namespace", "labels", "annotations"} {
				if d, ok := dm[f]; ok {
					if !subset(d, lm[f]) {
						return false, "metadata." + f
					}
				}
			}
			continue
		}
		if path, ok := firstDiff(k, dv, live[k]); !ok {
			return false, path
		}
	}
	return true, ""
}

func subset(d, l any) bool {
	_, ok := firstDiff("", d, l)
	return ok
}

// firstDiff compares desired ⊆ live and returns the path of the first
// difference (ok=false) .
func firstDiff(path string, d, l any) (string, bool) {
	switch dv := d.(type) {
	case nil:
		return "", true // null in a manifest means "not set"
	case map[string]any:
		lv, ok := l.(map[string]any)
		if !ok {
			return path, len(dv) == 0
		}
		for k, v := range dv {
			if p, ok := firstDiff(join(path, k), v, lv[k]); !ok {
				return p, false
			}
		}
		return "", true
	case []any:
		lv, ok := l.([]any)
		if !ok || len(lv) != len(dv) {
			return path, len(dv) == 0 && l == nil
		}
		for i := range dv {
			if p, ok := firstDiff(fmt.Sprintf("%s[%d]", path, i), dv[i], lv[i]); !ok {
				return p, false
			}
		}
		return "", true
	}
	if scalarEqual(d, l) {
		return "", true
	}
	return path, false
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// scalarEqual: numbers by value (YAML 2 and JSON 2.0 are one), quantities by
// amount ("0.5" CPU is "500m"), strings and booleans as they are.
func scalarEqual(d, l any) bool {
	if reflect.DeepEqual(d, l) {
		return true
	}
	if df, ok := number(d); ok {
		if lf, ok := number(l); ok {
			return df == lf
		}
	}
	ds, ok1 := d.(string)
	ls, ok2 := l.(string)
	if ok1 && ok2 {
		dq, err1 := resource.ParseQuantity(ds)
		lq, err2 := resource.ParseQuantity(ls)
		return err1 == nil && err2 == nil && dq.Cmp(lq) == 0
	}
	if ok1 {
		// intOrString written as a string of digits ("8080") vs 8080.
		if lf, ok := number(l); ok {
			return ds == fmt.Sprint(lf) || ds == fmt.Sprint(int64(lf))
		}
	}
	return false
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}
