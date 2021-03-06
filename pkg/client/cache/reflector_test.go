/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
)

type testLW struct {
	ListFunc  func() (runtime.Object, error)
	WatchFunc func(resourceVersion string) (watch.Interface, error)
}

func (t *testLW) List() (runtime.Object, error) { return t.ListFunc() }
func (t *testLW) Watch(resourceVersion string) (watch.Interface, error) {
	return t.WatchFunc(resourceVersion)
}

func TestReflector_watchHandlerError(t *testing.T) {
	s := NewStore()
	g := NewReflector(&testLW{}, &api.Pod{}, s)
	fw := watch.NewFake()
	go func() {
		fw.Stop()
	}()
	var resumeRV string
	err := g.watchHandler(fw, &resumeRV)
	if err == nil {
		t.Errorf("unexpected non-error")
	}
}

func TestReflector_watchHandler(t *testing.T) {
	s := NewStore()
	g := NewReflector(&testLW{}, &api.Pod{}, s)
	fw := watch.NewFake()
	s.Add("foo", &api.Pod{TypeMeta: api.TypeMeta{ID: "foo"}})
	s.Add("bar", &api.Pod{TypeMeta: api.TypeMeta{ID: "bar"}})
	go func() {
		fw.Add(&api.Service{TypeMeta: api.TypeMeta{ID: "rejected"}})
		fw.Delete(&api.Pod{TypeMeta: api.TypeMeta{ID: "foo"}})
		fw.Modify(&api.Pod{TypeMeta: api.TypeMeta{ID: "bar", ResourceVersion: "55"}})
		fw.Add(&api.Pod{TypeMeta: api.TypeMeta{ID: "baz", ResourceVersion: "32"}})
		fw.Stop()
	}()
	var resumeRV string
	err := g.watchHandler(fw, &resumeRV)
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}

	table := []struct {
		ID     string
		RV     string
		exists bool
	}{
		{"foo", "", false},
		{"rejected", "", false},
		{"bar", "55", true},
		{"baz", "32", true},
	}
	for _, item := range table {
		obj, exists := s.Get(item.ID)
		if e, a := item.exists, exists; e != a {
			t.Errorf("%v: expected %v, got %v", item.ID, e, a)
		}
		if !exists {
			continue
		}
		if e, a := item.RV, obj.(*api.Pod).ResourceVersion; e != a {
			t.Errorf("%v: expected %v, got %v", item.ID, e, a)
		}
	}

	// RV should send the last version we see.
	if e, a := "32", resumeRV; e != a {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestReflector_listAndWatch(t *testing.T) {
	createdFakes := make(chan *watch.FakeWatcher)

	// The ListFunc says that it's at revision 1. Therefore, we expect our WatchFunc
	// to get called at the beginning of the watch with 1, and again with 3 when we
	// inject an error.
	expectedRVs := []string{"1", "3"}
	lw := &testLW{
		WatchFunc: func(rv string) (watch.Interface, error) {
			fw := watch.NewFake()
			if e, a := expectedRVs[0], rv; e != a {
				t.Errorf("Expected rv %v, but got %v", e, a)
			}
			expectedRVs = expectedRVs[1:]
			// channel is not buffered because the for loop below needs to block. But
			// we don't want to block here, so report the new fake via a go routine.
			go func() { createdFakes <- fw }()
			return fw, nil
		},
		ListFunc: func() (runtime.Object, error) {
			return &api.PodList{TypeMeta: api.TypeMeta{ResourceVersion: "1"}}, nil
		},
	}
	s := NewFIFO()
	r := NewReflector(lw, &api.Pod{}, s)
	go r.listAndWatch()

	ids := []string{"foo", "bar", "baz", "qux", "zoo"}
	var fw *watch.FakeWatcher
	for i, id := range ids {
		if fw == nil {
			fw = <-createdFakes
		}
		sendingRV := strconv.FormatUint(uint64(i+2), 10)
		fw.Add(&api.Pod{TypeMeta: api.TypeMeta{ID: id, ResourceVersion: sendingRV}})
		if sendingRV == "3" {
			// Inject a failure.
			fw.Stop()
			fw = nil
		}
	}

	// Verify we received the right ids with the right resource versions.
	for i, id := range ids {
		pod := s.Pop().(*api.Pod)
		if e, a := id, pod.ID; e != a {
			t.Errorf("%v: Expected %v, got %v", i, e, a)
		}
		if e, a := strconv.FormatUint(uint64(i+2), 10), pod.ResourceVersion; e != a {
			t.Errorf("%v: Expected %v, got %v", i, e, a)
		}
	}

	if len(expectedRVs) != 0 {
		t.Error("called watchStarter an unexpected number of times")
	}
}

func TestReflector_listAndWatchWithErrors(t *testing.T) {
	mkPod := func(id string, rv string) *api.Pod {
		return &api.Pod{TypeMeta: api.TypeMeta{ID: id, ResourceVersion: rv}}
	}
	mkList := func(rv string, pods ...*api.Pod) *api.PodList {
		list := &api.PodList{TypeMeta: api.TypeMeta{ResourceVersion: rv}}
		for _, pod := range pods {
			list.Items = append(list.Items, *pod)
		}
		return list
	}
	table := []struct {
		list     *api.PodList
		listErr  error
		events   []watch.Event
		watchErr error
	}{
		{
			list: mkList("1"),
			events: []watch.Event{
				{watch.Added, mkPod("foo", "2")},
				{watch.Added, mkPod("bar", "3")},
			},
		}, {
			list: mkList("3", mkPod("foo", "2"), mkPod("bar", "3")),
			events: []watch.Event{
				{watch.Deleted, mkPod("foo", "4")},
				{watch.Added, mkPod("qux", "5")},
			},
		}, {
			listErr: fmt.Errorf("a list error"),
		}, {
			list:     mkList("5", mkPod("bar", "3"), mkPod("qux", "5")),
			watchErr: fmt.Errorf("a watch error"),
		}, {
			list: mkList("5", mkPod("bar", "3"), mkPod("qux", "5")),
			events: []watch.Event{
				{watch.Added, mkPod("baz", "6")},
			},
		}, {
			list: mkList("6", mkPod("bar", "3"), mkPod("qux", "5"), mkPod("baz", "6")),
		},
	}

	s := NewFIFO()
	for line, item := range table {
		if item.list != nil {
			// Test that the list is what currently exists in the store.
			current := s.List()
			checkMap := map[string]string{}
			for _, item := range current {
				pod := item.(*api.Pod)
				checkMap[pod.ID] = pod.ResourceVersion
			}
			for _, pod := range item.list.Items {
				if e, a := pod.ResourceVersion, checkMap[pod.ID]; e != a {
					t.Errorf("%v: expected %v, got %v for pod %v", line, e, a, pod.ID)
				}
			}
			if e, a := len(item.list.Items), len(checkMap); e != a {
				t.Errorf("%v: expected %v, got %v", line, e, a)
			}
		}
		watchRet, watchErr := item.events, item.watchErr
		lw := &testLW{
			WatchFunc: func(rv string) (watch.Interface, error) {
				if watchErr != nil {
					return nil, watchErr
				}
				watchErr = fmt.Errorf("second watch")
				fw := watch.NewFake()
				go func() {
					for _, e := range watchRet {
						fw.Action(e.Type, e.Object)
					}
					fw.Stop()
				}()
				return fw, nil
			},
			ListFunc: func() (runtime.Object, error) {
				return item.list, item.listErr
			},
		}
		r := NewReflector(lw, &api.Pod{}, s)
		r.listAndWatch()
	}
}
