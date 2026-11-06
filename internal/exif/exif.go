package exif

import (
	"math"
	"os"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// PhotoInfo 从图片 EXIF 中提取的 GPS/时间/方向/海拔信息。
type PhotoInfo struct {
	Latitude  *float64  // WGS-84 纬度
	Longitude *float64  // WGS-84 经度
	Altitude  *float64  // 海拔（米）
	Heading   *float64  // 相机方向角度（0=北, 90=东），来自 GPSImgDirection
	CreatedAt time.Time // 图片创建时间（DateTimeOriginal），零值表示无
}

// Parse 从图片文件读取 EXIF 并返回 PhotoInfo。文件不是 JPEG/TIFF 或无 EXIF 时返回空结构。
func Parse(path string) PhotoInfo {
	info := PhotoInfo{}

	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()

	x, err := exif.Decode(f)
	if err != nil {
		return info
	}

	// GPS 经纬度（WGS-84）
	if lat, lon, err := x.LatLong(); err == nil {
		info.Latitude = &lat
		info.Longitude = &lon
	}

	// 海拔：GPSAltitudeRef (0=海平面上, 1=海平面下) + GPSAltitude
	if altTag, err := x.Get(exif.GPSAltitude); err == nil {
		if num, den, err := altTag.Rat2(0); err == nil && den != 0 {
			a := float64(num) / float64(den)
			if refTag, err := x.Get(exif.GPSAltitudeRef); err == nil {
				if ref, err := refTag.Int(0); err == nil && ref == 1 {
					a = -a
				}
			}
			info.Altitude = &a
		}
	}

	// 相机方向：GPSImgDirection（图片拍摄时相机朝向）
	if dirTag, err := x.Get(exif.GPSImgDirection); err == nil {
		if num, den, err := dirTag.Rat2(0); err == nil && den != 0 {
			d := math.Mod(float64(num)/float64(den), 360)
			info.Heading = &d
		}
	}

	// 创建时间：DateTime() 优先（含时区处理）
	if t, err := x.DateTime(); err == nil {
		info.CreatedAt = t
	}

	return info
}

// FieldName 是 EXIF 字段名的类型别名（便于引用）。
type FieldName = exif.FieldName
