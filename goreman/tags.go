package goreman

import "reflect"

func GetTag(s interface{}, field, key string) string {
	t := reflect.TypeOf(s)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	f, ok := t.FieldByName(field)
	if !ok {
		return ""
	}
	return f.Tag.Get(key)
}
