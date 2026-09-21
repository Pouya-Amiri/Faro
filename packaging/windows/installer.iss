; Inno Setup Script for Faro
; Creates a modern 64-bit Windows installer for the Faro desktop client.

#ifndef AppVersion
  #error AppVersion must be provided (for example: ISCC.exe /DAppVersion=0.1.0 installer.iss)
#endif

[Setup]
AppId={{E6F7A9B1-23C4-4B68-912E-73D41657DE02}
AppName=Faro
AppVersion={#AppVersion}
AppPublisher=Faro
DefaultDirName={autopf}\Faro
DefaultGroupName=Faro
AllowNoIcons=yes
LicenseFile=..\..\LICENSE
OutputDir=..\..\dist_actions
OutputBaseFilename=Faro-{#AppVersion}-windows-x86_64-setup
SetupIconFile=..\..\build\windows\icon.ico
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
ArchitecturesAllowed=x64compatible
UninstallDisplayIcon={app}\Faro.exe
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
CloseApplications=yes
RestartApplications=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"
Name: "german"; MessagesFile: "compiler:Languages\German.isl"
Name: "french"; MessagesFile: "compiler:Languages\French.isl"
Name: "italian"; MessagesFile: "compiler:Languages\Italian.isl"
Name: "spanish"; MessagesFile: "compiler:Languages\Spanish.isl"
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
; The standalone server is distributed as its own release archive.
Source: "..\..\build\bin\Faro.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\THIRD_PARTY_NOTICES.txt"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\Faro"; Filename: "{app}\Faro.exe"
Name: "{group}\{cm:UninstallProgram,Faro}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\Faro"; Filename: "{app}\Faro.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\Faro.exe"; Description: "{cm:LaunchProgram,Faro}"; Flags: nowait postinstall skipifsilent
