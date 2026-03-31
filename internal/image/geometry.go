package image

import "math"

// SlotAspectRatio returns the required aspect ratio (width/height) for the
// given image slot. Returns 0 if the slot has no specific ratio requirement.
//
//	thumb:  1.0   (1:1 square)
//	fanart: 16/9  (16:9 widescreen)
//	banner: 5.4   (5.4:1 wide)
//	logo:   0     (no fixed ratio -- logos vary widely)
func SlotAspectRatio(slot string) float64 {
	switch slot {
	case "thumb":
		return 1.0
	case "fanart":
		return 16.0 / 9.0
	case "banner":
		return 5.4
	default:
		return 0
	}
}

// geometryTolerance is the default tolerance for aspect ratio matching.
// 10% allows for minor variations in provider-sourced images.
const geometryTolerance = 0.10

// GeometryResult holds the result of a geometry check for an uploaded image.
type GeometryResult struct {
	// NeedsCrop is true when the image aspect ratio does not match the slot requirement.
	NeedsCrop bool `json:"needs_crop"`

	// RequiredRatio is the aspect ratio (width/height) that the slot expects.
	// Zero when the slot has no fixed ratio requirement (e.g. logo).
	RequiredRatio float64 `json:"required_ratio"`

	// ActualRatio is the aspect ratio of the uploaded image.
	ActualRatio float64 `json:"actual_ratio"`

	// Width and Height are the dimensions of the uploaded image.
	Width  int `json:"width"`
	Height int `json:"height"`
}

// CheckGeometry determines whether the given image dimensions match the
// required aspect ratio for the target slot. A tolerance of 10% is applied
// to allow minor variations. Slots with no fixed ratio (logo) never require
// a crop. Images with zero dimensions are considered matching (unknown size).
func CheckGeometry(width, height int, slot string) GeometryResult {
	required := SlotAspectRatio(slot)

	result := GeometryResult{
		RequiredRatio: required,
		Width:         width,
		Height:        height,
	}

	// Cannot determine ratio with zero dimensions.
	if width <= 0 || height <= 0 {
		return result
	}

	result.ActualRatio = float64(width) / float64(height)

	// Slots with no fixed ratio never need cropping.
	if required == 0 {
		return result
	}

	// Check whether the actual ratio is within tolerance of the required ratio.
	if math.Abs(result.ActualRatio-required)/required > geometryTolerance {
		result.NeedsCrop = true
	}

	return result
}
