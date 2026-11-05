#!/usr/bin/env python3
"""把原始 ONNX 模型清理并量化为 INT8（dynamic），供浏览器 ONNX Runtime Web 使用。

执行：python3 scripts/prepare_models.py
产物：
  models/arcface_int8.onnx          人脸特征 (512-d)
  models/plate_det_int8.onnx        车牌检测 (YOLOv5-face, 640)
  models/plate_rec_int8.onnx        车牌字符识别 (CTC)
  models/chars.json                 字符表
"""
import os
import onnx
import numpy as np
import onnxruntime as ort
from onnxruntime.quantization import quantize_dynamic, QuantType

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODELS = os.path.join(ROOT, "models")


def strip_initializer_inputs(src, dst):
    """删除与 initializer 同名的 graph input，避免浏览器端要求额外输入。"""
    m = onnx.load(src)
    init_names = {i.name for i in m.graph.initializer}
    keep = [i for i in m.graph.input if i.name not in init_names]
    if len(keep) != len(m.graph.input):
        del m.graph.input[:]
        m.graph.input.extend(keep)
        onnx.save(m, dst)
        print(f"cleaned inputs: {src} -> {dst} ({len(keep)} input)")
    else:
        m = None
        if not os.path.exists(dst):
            onnx.save(onnx.load(src), dst)
        print(f"no clean needed: {dst}")


def quant(src, dst, ops):
    if os.path.exists(dst):
        print(f"exists: {os.path.basename(dst)}")
        return
    quantize_dynamic(src, dst, weight_type=QuantType.QInt8, op_types_to_quantize=ops)
    print(f"quantized {os.path.basename(src)} {os.path.getsize(src) >> 20}MB -> "
          f"{os.path.basename(dst)} {os.path.getsize(dst) >> 20}MB")


def check(path):
    s = ort.InferenceSession(path, providers=["CPUExecutionProvider"])
    print("  io:", [(i.name, i.shape) for i in s.get_inputs()], "->",
          [(o.name, o.shape) for o in s.get_outputs()])


def main():
    # 1. ArcFace: 112x112x3 RGB float，输出 512 维
    arc = os.path.join(MODELS, "arc.onnx")
    quant(arc, os.path.join(MODELS, "arcface_int8.onnx"), ["MatMul", "Gemm", "Conv"])

    # 2. 车牌检测
    det = os.path.join(MODELS, "y5fu_640x_sim.onnx")
    det_clean = os.path.join(MODELS, "plate_det.onnx")
    strip_initializer_inputs(det, det_clean)
    quant(det_clean, os.path.join(MODELS, "plate_det_int8.onnx"), ["Conv", "MatMul"])

    # 3. 车牌识别
    rec = os.path.join(MODELS, "rpv3_mdict_160_r3.onnx")
    rec_clean = os.path.join(MODELS, "plate_rec.onnx")
    strip_initializer_inputs(rec, rec_clean)
    quant(rec_clean, os.path.join(MODELS, "plate_rec_int8.onnx"), ["Conv", "MatMul"])

    for f in ["arcface_int8.onnx", "plate_det_int8.onnx", "plate_rec_int8.onnx"]:
        p = os.path.join(MODELS, f)
        print(f"{f}: {os.path.getsize(p) >> 20}MB")
        check(p)


def fetch_face_attrs():
    """下载人脸属性模型（性别/年龄，onnx-community 官方 INT8，浏览器兼容）。

    眼镜分类模型无现成 ONNX，由 scripts/export_glasses.py 从 PyTorch 权重转换。
    """
    import urllib.request
    base = "https://hf-mirror.com/onnx-community/fairface_%s_image_detection-ONNX/resolve/main/onnx/model_quantized.onnx"
    for kind, dst in [("age", "face_age.onnx"), ("gender", "face_gender.onnx")]:
        out = os.path.join(MODELS, dst)
        if os.path.exists(out):
            print(f"exists: {dst}")
            continue
        url = base % kind
        print(f"download {url} -> {dst}")
        urllib.request.urlretrieve(url, out)
        print(f"  {os.path.getsize(out) >> 20}MB")


if __name__ == "__main__":
    import sys
    if len(sys.argv) > 1 and sys.argv[1] == "attrs":
        fetch_face_attrs()
    else:
        main()
