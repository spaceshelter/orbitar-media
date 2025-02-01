#!/bin/bash

# Import environment variables
REQUIRED_SPACE=${REQUIRED_SPACE:-5} # Required space in GB

counter=0 # Counter to prevent infinite loop
while [ $counter -lt 100 ]; do
  AVAILABLE_SPACE=$(df -BG "/var/lib/orbitar_media_data" | awk 'NR==2 {print $4}' | tr -dc '0-9')
  echo "Available disk space: $AVAILABLE_SPACE GB"

  if (( AVAILABLE_SPACE >= REQUIRED_SPACE )); then
    echo "Required space is available. Cleanup is not required."
    break
  else
    echo "Cleaning up the least recently accessed directory..."
    ls -1turF "/var/lib/orbitar_media_data" | grep "/$" | sed 's/\/$//' | head -n 100 | xargs -I {} rm -rf "/var/lib/orbitar_media_data/{}"

  fi

  counter=$((counter+1))
done