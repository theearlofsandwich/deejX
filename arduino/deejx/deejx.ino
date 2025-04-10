#include <Arduino.h>
#include <Adafruit_GFX.h>
#include <Adafruit_SSD1306.h>
#include <Wire.h>
#include <ResponsiveAnalogRead.h>

#define SLIDER1 "Master"
#define SLIDER2 "Unbound"
#define SLIDER3 "Discord"

const int NUM_SLIDERS = 2;

#define ENCODER_CLK 2
#define ENCODER_DT 3
#define ENCODER_SW 4

#define SHOWSCREENS true

#define TCAADDR 0x70

#define SCREEN_ADDRESS 0x3C

#define SCREEN_WIDTH 128 // OLED display width, in pixels
//#define SCREEN_HEIGHT 64 // OLED display height, in pixels
#define SCREEN_HEIGHT 32 // OLED display height, in pixels
#define OLED_RESET     -1

Adafruit_SSD1306 display(SCREEN_WIDTH, SCREEN_HEIGHT, &Wire, OLED_RESET);


const byte numChars = 10;
char receivedChars[numChars];
char tempChars[numChars];
boolean newData = false;

int mute = 0;
int masterVolume = 0;

int currentStateCLK;
int lastClk = HIGH;
int lastButtonState = 0;

int analogSliderValues[NUM_SLIDERS];
const int analogInputs[NUM_SLIDERS] = {A0, A1};
char outputBuffer[20];

int screenSliderValues[NUM_SLIDERS];
boolean dataChanged = true;


ResponsiveAnalogRead analogOne(A0, true);
ResponsiveAnalogRead analogTwo(A1, true);

void setup() {
  // put your setup code here, to run once:
  Serial.begin(9600);

  analogOne.setActivityThreshold(15);
  analogTwo.setActivityThreshold(15);
  // analogOne.enableEdgeSnap();
  // analogTwo.enableEdgeSnap();
  
  // if(SHOWSCREENS && !display.begin(SSD1306_SWITCHCAPVCC, SCREEN_ADDRESS)) {
  //   Serial.println(F("SSD1306 allocation failed"));
  //   for(;;); // Don't proceed, loop forever
  // }
  if(SHOWSCREENS) {
    initDisplay(0);
    initDisplay(1);
    initDisplay(2);
  }

  // Set encoder pins as inputs
  pinMode(ENCODER_CLK ,INPUT);
  pinMode(ENCODER_DT,INPUT);
  pinMode(ENCODER_SW, INPUT_PULLUP);


  // Read the initial state of CLK
  attachInterrupt(digitalPinToInterrupt(ENCODER_CLK), readEncoderTurn, FALLING);

  for (int i = 0; i < NUM_SLIDERS; i++) {
    pinMode(analogInputs[i], INPUT);
  }
}


void loop() {
  // put your main code here, to run repeatedly:

  // Let's read the current system volume
  receiveWithStartEndMarkers();
  if (newData == true) {
    strcpy(tempChars, receivedChars);
        // this temporary copy is necessary to protect the original data
        //   because strtok() used in parseData() replaces the commas with \0
    parseReceivedData();
    dataChanged = true;
    newData = false;
  }

  // Read the button state
  int btnState = digitalRead(ENCODER_SW);
  //If we detect LOW signal, button is pressed
  if (btnState == LOW) {
    lastButtonState = 1;
  } else {
    if(lastButtonState == 1) {
      // Button released
      sprintf(outputBuffer, "^|%d|%d", analogSliderValues[0], analogSliderValues[1]);
      Serial.println(outputBuffer);
      lastButtonState = 0;
      dataChanged = true;      
    }
  }
  
  updateSliderValues();
  sendSliderValues();

  if(SHOWSCREENS && dataChanged) {
    updateDisplay(0);
    updateDisplay(1);
    updateDisplay(2);
    dataChanged = false;
  }

  delay(10);

}

void initDisplay(int displayId) {
  tcaselect(displayId);
  display.begin(SSD1306_SWITCHCAPVCC, SCREEN_ADDRESS);
  display.display();
  display.clearDisplay();
}

void updateDisplay(int displayId) {
  tcaselect(displayId);

  display.clearDisplay();
  
  display.setTextSize(1);
  display.setTextColor(SSD1306_WHITE);
  display.setCursor(5, 0);

  switch (displayId) {
    case 0:
      display.println(F(SLIDER1));
      drawBars(masterVolume, 100);
      break;
    case 1:
      display.println(F(SLIDER2));
      drawBars(analogSliderValues[0], 100);
      break;
    case 2:
      display.println(F(SLIDER3));
      drawBars(analogSliderValues[1], 100);
      break;
  }

  display.display();
}

void drawBars(int value, int maxValue) {
  display.drawRoundRect(0, 12, display.width(), display.height() - 12, 5, SSD1306_WHITE);

  int boxWidth = int(map(value, 0, maxValue, 0, display.width() - 4));
  if(mute) {
    display.drawRoundRect(2, 14, boxWidth, display.height() - 16, 5, SSD1306_WHITE);
  } else {
    display.fillRoundRect(2, 14, boxWidth, display.height() - 16, 5, SSD1306_WHITE);
  }
}

void readEncoderTurn() {

  int newClk = digitalRead(ENCODER_CLK);

  if (newClk != lastClk) {
    int dtValue = digitalRead(ENCODER_DT);
    if (newClk == LOW && dtValue == HIGH) {
      sprintf(outputBuffer, "+|%d|%d", analogSliderValues[0], analogSliderValues[1]);
      Serial.println(outputBuffer);
    }
    if (newClk == LOW && dtValue == LOW) {
      sprintf(outputBuffer, "-|%d|%d", analogSliderValues[0], analogSliderValues[1]);
      Serial.println(outputBuffer);
    }
    dataChanged = true;
  }
}

void updateSliderValues() {
  analogOne.update();
  analogTwo.update();

  analogSliderValues[0] = map(analogOne.getValue(), 0, 1023, 0, 100);
  analogSliderValues[1] = map(analogTwo.getValue(), 0, 1023, 0, 100);


  for (int i = 0; i < NUM_SLIDERS; i++) {
    if(analogSliderValues[i] != screenSliderValues[i]) {
      screenSliderValues[i] = analogSliderValues[i];
      dataChanged = true;
    }
  }
}

void sendSliderValues() {
  sprintf(outputBuffer, "=|%d|%d", analogSliderValues[0], analogSliderValues[1]);
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
                if (ndx >= numChars) {
                    ndx = numChars - 1;
                }
            }
            else {
                receivedChars[ndx] = '\0'; // terminate the string
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

void parseReceivedData() { // split the data into its parts
    char * strtokIndx; // this is used by strtok() as an index

    strtokIndx = strtok(tempChars,"|");  // get the first part - the string
    mute = atoi(strtokIndx);

    strtokIndx = strtok(NULL, "|"); // this continues where the previous call left off
    masterVolume = atoi(strtokIndx);     // convert this part to an integer
}

void tcaselect(uint8_t i) {
  if (i > 7) return;
 
  Wire.beginTransmission(TCAADDR);
  Wire.write(1 << i);
  Wire.endTransmission();  
}
