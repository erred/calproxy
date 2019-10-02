package ical

// THIS FILE IS AUTO-GENERATED BY internal/cmd/gentypes/gentypes.go
// DO NOT EDIT. ALL CHANGES WILL BE LOST

import (
	"bytes"
	"strings"

	"github.com/pkg/errors"
)

type Event struct {
	entries EntryList
	props   *PropertySet
}

func NewEvent() *Event {
	return &Event{
		props: NewPropertySet(),
	}
}

func (v *Event) String() string {
	var buf bytes.Buffer
	NewEncoder(&buf).Encode(v)
	return buf.String()
}

func (v Event) Type() string {
	return "VEVENT"
}

func (v *Event) AddEntry(e Entry) error {
	v.entries.Append(e)
	return nil
}

func (v *Event) Entries() <-chan Entry {
	return v.entries.Iterator()
}

func (v *Event) GetProperty(name string) (*Property, bool) {
	return v.props.GetFirst(name)
}

func (v *Event) Properties() <-chan *Property {
	return v.props.Iterator()
}

func (v *Event) AddProperty(key, value string, options ...PropertyOption) error {
	var params Parameters
	var force bool
	for _, option := range options {
		switch option.Name() {
		case "Parameters":
			params = option.Get().(Parameters)
		case "Force":
			force = option.Get().(bool)
		}
	}

	switch key = strings.ToLower(key); key {
	case "class", "created", "description", "dtstamp", "dtstart", "dtend", "duration", "geo", "last-modified", "location", "organizer", "priority", "sequence", "status", "summary", "transp", "uid", "url", "recurrence-id":
		v.props.Set(NewProperty(key, value, params))
	default:
		if strings.HasPrefix(key, "x-") || force {
			v.props.Append(NewProperty(key, value, params))
		} else {
			return errors.Errorf(`invalid property %s`, key)
		} /* end if */
	}
	return nil
}

func (v *Event) MarshalJSON() ([]byte, error) {
	var dst bytes.Buffer
	if err := NewJSONEncoder(&dst).Encode(v); err != nil {
		return nil, errors.Wrap(err, `failed to encode json`)
	}
	return dst.Bytes(), nil
}
