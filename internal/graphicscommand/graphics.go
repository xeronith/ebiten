// Copyright 2022 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package graphicscommand

import (
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
)

type Updater interface {
	Update(updateCount int, outsideWidth, outsideHeight float64, deviceScaleFactor float64, needsClearingScreen bool, framebufferYDirection graphicsdriver.YDirection) error
}

func Update(updater Updater, updateCount int, outsideWidth, outsideHeight float64, deviceScaleFactor float64) error {
	return updater.Update(updateCount, outsideWidth, outsideHeight, deviceScaleFactor, graphicsDriver().NeedsClearingScreen(), graphicsDriver().FramebufferYDirection())
}

func ForceUpdate(updater Updater, outsideWidth, outsideHeight float64, deviceScaleFactor float64) error {
	if err := updater.Update(1, outsideWidth, outsideHeight, deviceScaleFactor, graphicsDriver().NeedsClearingScreen(), graphicsDriver().FramebufferYDirection()); err != nil {
		return err
	}
	if graphicsDriver().IsDirectX() {
		if err := updater.Update(0, outsideWidth, outsideHeight, deviceScaleFactor, graphicsDriver().NeedsClearingScreen(), graphicsDriver().FramebufferYDirection()); err != nil {
			return err
		}
	}
	return nil
}

// TODO: Reduce these 'getter' global functions if possible.

func NeedsInvertY() bool {
	return graphicsDriver().FramebufferYDirection() != graphicsDriver().NDCYDirection()
}

func NeedsRestoring() bool {
	return graphicsDriver().NeedsRestoring()
}

func IsGL() bool {
	return graphicsDriver().IsGL()
}

func SetVsyncEnabled(enabled bool) {
	graphicsDriver().SetVsyncEnabled(enabled)
}

func SetTransparent(transparent bool) {
	graphicsDriver().SetTransparent(transparent)
}

func SetFullscreen(fullscreen bool) {
	graphicsDriver().SetFullscreen(fullscreen)
}

func SetWindow(window uintptr) {
	if g, ok := graphicsDriver().(interface{ SetWindow(uintptr) }); ok {
		g.SetWindow(window)
	}
}
