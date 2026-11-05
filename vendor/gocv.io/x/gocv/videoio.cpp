#include "videoio.h"

// VideoCapture - 我们不使用视频 IO，用 stub 避免 OpenCV 4.11 兼容问题
VideoCapture VideoCapture_New() { return nullptr; }
void VideoCapture_Close(VideoCapture v) {}
bool VideoCapture_Open(VideoCapture v, const char* uri) { return false; }
bool VideoCapture_OpenWithAPI(VideoCapture v, const char* uri, int apiPreference) { return false; }
bool VideoCapture_OpenWithAPIParams(VideoCapture v, const char* uri, int apiPreference, int *paramsv, int paramsc) { return false; }
bool VideoCapture_OpenDevice(VideoCapture v, int device) { return false; }
bool VideoCapture_OpenDeviceWithAPI(VideoCapture v, int device, int apiPreference) { return false; }
bool VideoCapture_OpenDeviceWithAPIParams(VideoCapture v, int device, int apiPreference, int *paramsv, int paramsc) { return false; }
void VideoCapture_Set(VideoCapture v, int prop, double param) {}
double VideoCapture_Get(VideoCapture v, int prop) { return 0; }
int VideoCapture_IsOpened(VideoCapture v) { return 0; }
int VideoCapture_Read(VideoCapture v, Mat buf) { return 0; }
void VideoCapture_Grab(VideoCapture v, int skip) {}
int VideoCapture_Retrieve(VideoCapture v, Mat buf) { return 0; }

// VideoWriter
VideoWriter VideoWriter_New() { return nullptr; }
void VideoWriter_Close(VideoWriter vw) {}
void VideoWriter_Open(VideoWriter vw, const char* name, const char* codec, double fps, int width, int height, bool isColor) {}
int VideoWriter_IsOpened(VideoWriter vw) { return 0; }
void VideoWriter_Write(VideoWriter vw, Mat img) {}
