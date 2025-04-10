#include <Wire.h>
#include <Adafruit_GFX.h>
#include <Adafruit_SSD1306.h>
#include <ResponsiveAnalogRead.h>
#include <avr/wdt.h>

// Configuration constants
#define CONFIG_NUM_SLIDERS 2
#define CONFIG_BAUD_RATE 9600
#define CONFIG_ANALOG_THRESHOLD 15
#define CONFIG_KEEPALIVE_TIMEOUT 10000

// Pin definitions
#define PIN_ENCODER_CLK 2
#define PIN_ENCODER_DT 3
#define PIN_ENCODER_SW 4

// Display configuration
#define DISPLAY_WIDTH 128
#define DISPLAY_HEIGHT 32
#define DISPLAY_RESET_PIN -1
#define SCREEN_ADDRESS 0x3C
#define TCAADDR 0x70

// Buffer sizes
#define SERIAL_BUFFER_SIZE 100
#define SLIDER_NAME_LENGTH 20
#define MAX_SLIDERS 4

// Command characters
#define CMD_EQUAL '='
#define CMD_PLUS '+'
#define CMD_MINUS '-'
#define CMD_CARET '^'

// Global variables
Adafruit_SSD1306 display(DISPLAY_WIDTH, DISPLAY_HEIGHT, &Wire, DISPLAY_RESET_PIN);
char outputBuffer[20];
char receivedChars[SERIAL_BUFFER_SIZE];
char tempChars[SERIAL_BUFFER_SIZE];
boolean newData = false;
unsigned long keepAlive = 0;

struct DeejState {
    // Encoder state
    int currentStateCLK;
    int lastClk;
    int lastButtonState;
    unsigned long lastButtonPress;
    
    // Slider state
    int analogSliderValues[CONFIG_NUM_SLIDERS];
    int screenSliderValues[CONFIG_NUM_SLIDERS];
    
    // Display state
    bool screensActive;
    bool dataChanged;
    
    // System state
    int mute;
    int masterVolume;
    unsigned long lastKeepAlive;
    
    // Slider names
    char sliderNames[MAX_SLIDERS][SLIDER_NAME_LENGTH];
    
    // Debounce configuration
    static const unsigned long DEBOUNCE_DELAY = 50;
    
    void init() {
        currentStateCLK = HIGH;
        lastClk = HIGH;
        lastButtonState = 0;
        lastButtonPress = 0;
        screensActive = true;
        dataChanged = true;
        mute = 0;
        masterVolume = 0;
        lastKeepAlive = 0;
        
        memset(analogSliderValues, 0, sizeof(analogSliderValues));
        memset(screenSliderValues, 0, sizeof(screenSliderValues));
        
        for (int i = 0; i < MAX_SLIDERS; i++) {
            memset(sliderNames[i], 0, SLIDER_NAME_LENGTH);
        }
    }
};

DeejState state;

// Responsible analog readers
ResponsiveAnalogRead analogReaders[CONFIG_NUM_SLIDERS] = {
    ResponsiveAnalogRead(A0, true),
    ResponsiveAnalogRead(A1, true)
};

void setup() {
    // Initialize watchdog timer
    wdt_disable();
    wdt_enable(WDTO_1S);
    
    Serial.begin(CONFIG_BAUD_RATE);
    state.init();
    
    // Initialize analog readers
    for (int i = 0; i < CONFIG_NUM_SLIDERS; i++) {
        analogReaders[i].setActivityThreshold(CONFIG_ANALOG_THRESHOLD);
    }
    
    // Initialize encoder pins
    pinMode(PIN_ENCODER_CLK, INPUT);
    pinMode(PIN_ENCODER_DT, INPUT);
    pinMode(PIN_ENCODER_SW, INPUT_PULLUP);
    
    attachInterrupt(digitalPinToInterrupt(PIN_ENCODER_CLK), readEncoderTurn, FALLING);
    
    // Initialize displays
    for (int i = 0; i < 3; i++) {
        if (!initDisplay(i)) {
            Serial.print(F("Display "));
            Serial.print(i);
            Serial.println(F(" initialization failed"));
        }
    }
}

void loop() {
    wdt_reset(); // Reset watchdog timer
    
    receiveWithStartEndMarkers();
    if (newData) {
        strcpy(tempChars, receivedChars);
        parseReceivedData();
        newData = false;
    }
    
    handleEncoder();
    updateSliderValues();
    
    // Check keepalive timeout
    if (millis() - keepAlive > CONFIG_KEEPALIVE_TIMEOUT) {
        if (state.screensActive) {
            state.screensActive = false;
            for (int i = 0; i < 3; i++) {
                tcaselect(i);
                display.clearDisplay();
                display.display();
            }
        }
    }
    
    if (state.screensActive && state.dataChanged) {
        updateDisplay(0);
        updateDisplay(1);
        updateDisplay(2);
        state.dataChanged = false;
    }
    
    delay(10);
}

void handleEncoder() {
    int btnState = digitalRead(PIN_ENCODER_SW);
    unsigned long currentTime = millis();
    
    if (btnState == LOW && 
        state.lastButtonState == HIGH && 
        (currentTime - state.lastButtonPress) > DeejState::DEBOUNCE_DELAY) {
        
        sprintf(outputBuffer, "^|%d|%d", 
            state.analogSliderValues[0], 
            state.analogSliderValues[1]);
        Serial.println(outputBuffer);
        state.dataChanged = true;
        state.lastButtonPress = currentTime;
    }
    state.lastButtonState = btnState;
}

void readEncoderTurn() {
  int newClk = digitalRead(PIN_ENCODER_CLK);
  
  if (newClk != state.lastClk) {
      int dtValue = digitalRead(PIN_ENCODER_DT);
      char command = (newClk == LOW && dtValue == HIGH) ? '+' : '-';
      
      if (command == '+' || command == '-') {
          sprintf(outputBuffer, "%c|%d|%d", 
              command,
              state.analogSliderValues[0], 
              state.analogSliderValues[1]);
          Serial.println(outputBuffer);
          state.dataChanged = true;
      }
  }
  state.lastClk = newClk;
}

bool initDisplay(int displayId) {
    if (displayId >= 3) {
        Serial.println(F("Error: Invalid display ID"));
        return false;
    }

    tcaselect(displayId);
    
    if (!display.begin(SSD1306_SWITCHCAPVCC, SCREEN_ADDRESS)) {
        Serial.print(F("Display "));
        Serial.print(displayId);
        Serial.println(F(" initialization failed"));
        return false;
    }
    
    display.clearDisplay();
    display.display();
    return true;
}

void updateDisplay(int displayId) {
    tcaselect(displayId);
    display.clearDisplay();
    
    display.setTextSize(1);
    display.setTextColor(SSD1306_WHITE);
    display.setCursor(5, 0);

    switch (displayId) {
        case 0:
            display.println(state.sliderNames[0]);
            drawBars(state.masterVolume, 100);
            break;
        case 1:
            display.println(state.sliderNames[1]);
            drawBars(state.analogSliderValues[0], 100);
            break;
        case 2:
            display.println(state.sliderNames[2]);
            drawBars(state.analogSliderValues[1], 100);
            break;
    }

    display.display();
}

void drawBars(int value, int maxValue) {
    display.drawRoundRect(0, 12, display.width(), display.height() - 12, 5, SSD1306_WHITE);

    int boxWidth = int(map(value, 0, maxValue, 0, display.width() - 4));
    if (state.mute) {
        display.drawRoundRect(2, 14, boxWidth, display.height() - 16, 5, SSD1306_WHITE);
    } else {
        display.fillRoundRect(2, 14, boxWidth, display.height() - 16, 5, SSD1306_WHITE);
    }
}

void updateSliderValues() {
    for (int i = 0; i < CONFIG_NUM_SLIDERS; i++) {
        analogReaders[i].update();
        int newValue = map(analogReaders[i].getValue(), 0, 1023, 0, 100);
        newValue = constrain(newValue, 0, 100);
        
        if (newValue != state.analogSliderValues[i]) {
            state.analogSliderValues[i] = newValue;
            state.screenSliderValues[i] = newValue;
            state.dataChanged = true;
        }
    }
    
    if (state.dataChanged) {
        sendSliderValues();
    }
}

void sendSliderValues() {
    sprintf(outputBuffer, "=|%d|%d", 
        state.analogSliderValues[0], 
        state.analogSliderValues[1]);
    Serial.println(outputBuffer);
}

void receiveWithStartEndMarkers() {
    static boolean recvInProgress = false;
    static byte ndx = 0;
    char startMarker = '<';
    char endMarker = '>';
    char rc;

    while (Serial.available() > 0 && newData == false) {
        rc = Serial.read();

        if (recvInProgress == true) {
            if (rc != endMarker) {
                receivedChars[ndx] = rc;
                ndx++;
                if (ndx >= SERIAL_BUFFER_SIZE) {
                    ndx = SERIAL_BUFFER_SIZE - 1;
                }
            }
            else {
                receivedChars[ndx] = '\0';
                recvInProgress = false;
                ndx = 0;
                newData = true;
            }
        }
        else if (rc == startMarker) {
            recvInProgress = true;
        }
    }
}

void parseReceivedData() {
    if (strlen(tempChars) < 1) {
        Serial.println(F("Error: Empty command received"));
        return;
    }

    char command = tempChars[0];
    char* data = tempChars + 1;

    switch (command) {
        case '!': {
            char *token = strtok(data, "|");
            if (token == NULL) {
                Serial.println(F("Error: Invalid mute command format"));
                return;
            }
            
            int newMute = constrain(atoi(token), 0, 1);
            
            token = strtok(NULL, "|");
            if (token == NULL) {
                Serial.println(F("Error: Invalid volume command format"));
                return;
            }
            
            int newVolume = constrain(atoi(token), 0, 100);
            
            state.mute = newMute;
            state.masterVolume = newVolume;
            state.dataChanged = true;
            break;
        }

        case '^': {
            int i = 0;
            char *token = strtok(data, "|");
            while (token != NULL && i < MAX_SLIDERS) {
                size_t tokenLen = strlen(token);
                size_t copyLen = min(tokenLen, (size_t)SLIDER_NAME_LENGTH - 1);
                memcpy(state.sliderNames[i], token, copyLen);
                state.sliderNames[i][copyLen] = '\0';
                i++;
                token = strtok(NULL, "|");
            }
            state.dataChanged = true;
            Serial.println(F("Parsed name list"));
            break;
        }

        case '#': {
            keepAlive = millis();
            if (!state.screensActive) {
                state.screensActive = true;
                state.dataChanged = true;
            }
            Serial.println(F("Keep-alive signal received"));
            break;
        }

        default:
            Serial.println(F("Unknown command"));
            break;
    }
}

void tcaselect(uint8_t i) {
    if (i > 7) return;
    Wire.beginTransmission(TCAADDR);
    Wire.write(1 << i);
    Wire.endTransmission();
}