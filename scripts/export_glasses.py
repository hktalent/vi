#!/usr/bin/env python3
"""把 youngp5/eyeglasses_detection (ViT-base) 转成浏览器可用的 ONNX INT8。

用法:
  1. 下载权重:
     curl -L -o /tmp/glasses_pt.bin \
       https://hf-mirror.com/youngp5/eyeglasses_detection/resolve/main/pytorch_model.bin
  2. 转换+量化:
     USE_TF=0 USE_FLAX=0 python3 scripts/export_glasses.py
产物: models/face_glasses.onnx (~89MB, 动态 INT8, MatMulInteger, ORT Web WASM 兼容)
注意: 需 torch + transformers (USE_TF=0 USE_FLAX=0 避开本机坏掉的 TF 导入)。
"""
import json, os, shutil
import numpy as np
import torch

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "models", "face_glasses.onnx")
SRC_BIN = os.environ.get("GLASSES_PT", "/tmp/glasses_pt.bin")
WORK = "/tmp/glasses_pt"
FP32 = "/tmp/glasses_fp32.onnx"

os.makedirs(WORK, exist_ok=True)
config = {
    "_name_or_path": "google/vit-base-patch16-224-in21k",
    "architectures": ["ViTForImageClassification"],
    "hidden_size": 768, "num_hidden_layers": 12, "num_attention_heads": 12,
    "intermediate_size": 3072, "hidden_act": "gelu", "patch_size": 16,
    "image_size": 224, "num_channels": 3, "qkv_bias": True,
    "layer_norm_eps": 1e-12, "encoder_stride": 16,
    "id2label": {"0": "glasses", "1": "no_glasses"},
    "label2id": {"glasses": 0, "no_glasses": 1},
    "model_type": "vit", "torch_dtype": "float32",
}
with open(os.path.join(WORK, "config.json"), "w") as f:
    json.dump(config, f, indent=2)
if not os.path.exists(os.path.join(WORK, "pytorch_model.bin")):
    shutil.copy(SRC_BIN, os.path.join(WORK, "pytorch_model.bin"))

from transformers import ViTForImageClassification
model = ViTForImageClassification.from_pretrained(WORK)
model.eval()

if not os.path.exists(FP32):
    dummy = torch.zeros(1, 3, 224, 224)
    torch.onnx.export(
        model, (dummy,), FP32,
        input_names=["pixel_values"], output_names=["logits"],
        dynamic_axes={"pixel_values": {0: "batch"}, "logits": {0: "batch"}},
        opset_version=14, do_constant_folding=True,
    )

import onnxruntime as ort
sess = ort.InferenceSession(FP32, providers=["CPUExecutionProvider"])
rng = np.random.RandomState(0)
x = rng.rand(2, 3, 224, 224).astype(np.float32) * 2 - 1
with torch.no_grad():
    ref = model(torch.from_numpy(x)).logits.numpy()
out = sess.run(["logits"], {"pixel_values": x})[0]
assert np.abs(out - ref).max() < 1e-4, "fp32 export mismatch"

# 只量化 MatMul/Gemm（量化 Conv 会生成 ORT 不支持的 ConvInteger）
from onnxruntime.quantization import quantize_dynamic, QuantType
quantize_dynamic(FP32, OUT, weight_type=QuantType.QInt8, per_channel=True,
                 reduce_range=True, op_types_to_quantize=["MatMul", "Gemm"])

sess2 = ort.InferenceSession(OUT, providers=["CPUExecutionProvider"])
out2 = sess2.run(["logits"], {"pixel_values": x})[0]
assert (out2.argmax(1) == ref.argmax(1)).all(), "int8 argmax mismatch"
print("OK:", OUT, os.path.getsize(OUT) >> 20, "MB")
